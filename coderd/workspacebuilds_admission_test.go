package coderd_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
)

type cancelAdmissionEnv struct {
	db     database.Store
	client *codersdk.Client
	orgID  uuid.UUID
	// createWaiting creates a workspace whose START waits for admission.
	createWaiting func(t *testing.T) codersdk.Workspace
}

// newCancelAdmissionEnv starts coderd with a managed template. No
// provisioner runs after the template import, so the test acquires jobs
// itself and controls the race.
func newCancelAdmissionEnv(t *testing.T) cancelAdmissionEnv {
	t.Helper()
	db, ps := dbtestutil.NewDB(t)
	client, provisioner := coderdtest.NewWithProvisionerCloser(t, &coderdtest.Options{
		Database: db,
		Pubsub:   ps,
	})
	owner := coderdtest.CreateFirstUser(t, client)
	version := coderdtest.CreateTemplateVersion(t, client, owner.OrganizationID, admissiontest.ProfileResponses())
	coderdtest.AwaitTemplateVersionJobCompleted(t, client, version.ID)
	require.NoError(t, provisioner.Close())
	template := coderdtest.CreateTemplate(t, client, owner.OrganizationID, version.ID)
	admissiontest.Manage(t, db, template.ID, admissiontest.ProfileParameter)
	admissiontest.Entitle(t, db, owner.UserID, template.ID, "small")
	return cancelAdmissionEnv{
		db:     db,
		client: client,
		orgID:  owner.OrganizationID,
		createWaiting: func(t *testing.T) codersdk.Workspace {
			t.Helper()
			return coderdtest.CreateWorkspace(t, client, template.ID, func(req *codersdk.CreateWorkspaceRequest) {
				req.RichParameterValues = admissiontest.Profile("small")
			})
		},
	}
}

func TestCancelWorkspaceBuildWorkspaceAdmission(t *testing.T) {
	t.Parallel()

	for _, expectStatus := range []codersdk.CancelWorkspaceBuildStatus{"", codersdk.CancelWorkspaceBuildStatusPending} {
		t.Run(fmt.Sprintf("Waiting%s", expectStatus), func(t *testing.T) {
			t.Parallel()
			env := newCancelAdmissionEnv(t)
			db, client := env.db, env.client
			ctx := testutil.Context(t, testutil.WaitLong)
			workspace := env.createWaiting(t)

			err := client.CancelWorkspaceBuild(ctx, workspace.LatestBuild.ID, codersdk.CancelWorkspaceBuildParams{ExpectStatus: expectStatus})
			require.NoError(t, err)

			job, err := db.GetProvisionerJobByID(ctx, workspace.LatestBuild.Job.ID)
			require.NoError(t, err)
			require.Equal(t, database.ProvisionerJobStatusCanceled, job.JobStatus)
			admission := admissiontest.Admission(t, db, job.ID)
			require.Equal(t, database.WorkspaceAdmissionStatusCanceled, admission.Status)

			// A canceled request can never be acquired.
			_, err = admissiontest.Acquire(ctx, db, env.orgID)
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}

	// The provisioner and the cancel endpoint race under the job row lock.
	// Either the job is canceled before it starts and its admission ends, or
	// the provisioner acquired it first and the cancel becomes a running
	// cancellation with the admission left acquired.
	t.Run("AcquireRace", func(t *testing.T) {
		t.Parallel()
		env := newCancelAdmissionEnv(t)
		db, client := env.db, env.client
		for i := range 10 {
			ctx := testutil.Context(t, testutil.WaitLong)
			workspace := env.createWaiting(t)
			jobID := workspace.LatestBuild.Job.ID
			admissiontest.Grant(t, db, nil, jobID)

			var (
				acquired   database.ProvisionerJob
				acquireErr error
				cancelErr  error
				start      = make(chan struct{})
				done       = make(chan struct{}, 2)
			)
			go func() {
				defer func() { done <- struct{}{} }()
				<-start
				acquired, acquireErr = admissiontest.Acquire(ctx, db, env.orgID)
			}()
			go func() {
				defer func() { done <- struct{}{} }()
				<-start
				cancelErr = client.CancelWorkspaceBuild(ctx, workspace.LatestBuild.ID, codersdk.CancelWorkspaceBuildParams{})
			}()
			close(start)
			<-done
			<-done

			require.NoError(t, cancelErr, "iteration %d", i)
			job, err := db.GetProvisionerJobByID(ctx, jobID)
			require.NoError(t, err)
			admission := admissiontest.Admission(t, db, jobID)
			require.True(t, job.CanceledAt.Valid, "iteration %d", i)
			if acquireErr == nil {
				require.Equal(t, jobID, acquired.ID, "iteration %d", i)
				require.False(t, job.CompletedAt.Valid, "iteration %d: running cancellation", i)
				require.Equal(t, database.WorkspaceAdmissionStatusAcquired, admission.Status, "iteration %d", i)
				// Finish the running job as the provisioner would.
				err = db.UpdateProvisionerJobWithCompleteByID(ctx, database.UpdateProvisionerJobWithCompleteByIDParams{
					ID:          jobID,
					UpdatedAt:   job.UpdatedAt,
					CompletedAt: sql.NullTime{Time: job.UpdatedAt, Valid: true},
					Error:       sql.NullString{},
					ErrorCode:   sql.NullString{},
				})
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, acquireErr, sql.ErrNoRows, "iteration %d", i)
				require.False(t, job.StartedAt.Valid, "iteration %d", i)
				require.Equal(t, database.ProvisionerJobStatusCanceled, job.JobStatus, "iteration %d", i)
				require.Equal(t, database.WorkspaceAdmissionStatusCanceled, admission.Status, "iteration %d", i)
			}
		}
	})
}
