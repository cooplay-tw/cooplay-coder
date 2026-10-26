package coderd_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
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

// TestWorkspaceBuildAdmissionResponses verifies that every workspace build
// response shows the admission of a managed START build to a member who may
// read the build, although members hold no permission on admission records,
// and that builds without admission omit the field.
//
//nolint:tparallel // Subtests run in order through the admission lifecycle.
func TestWorkspaceBuildAdmissionResponses(t *testing.T) {
	t.Parallel()

	db, ps := dbtestutil.NewDB(t)
	ownerClient, provisioner := coderdtest.NewWithProvisionerCloser(t, &coderdtest.Options{
		Database: db,
		Pubsub:   ps,
	})
	owner := coderdtest.CreateFirstUser(t, ownerClient)
	managedVersion := coderdtest.CreateTemplateVersion(t, ownerClient, owner.OrganizationID, admissiontest.ProfileResponses())
	coderdtest.AwaitTemplateVersionJobCompleted(t, ownerClient, managedVersion.ID)
	plainVersion := coderdtest.CreateTemplateVersion(t, ownerClient, owner.OrganizationID, nil)
	coderdtest.AwaitTemplateVersionJobCompleted(t, ownerClient, plainVersion.ID)
	// Without a provisioner every build stays pending, so the test controls
	// the admission state.
	require.NoError(t, provisioner.Close())
	managed := coderdtest.CreateTemplate(t, ownerClient, owner.OrganizationID, managedVersion.ID)
	plain := coderdtest.CreateTemplate(t, ownerClient, owner.OrganizationID, plainVersion.ID)
	admissiontest.Manage(t, db, managed.ID, admissiontest.ProfileParameter)

	memberClient, member := coderdtest.CreateAnotherUser(t, ownerClient, owner.OrganizationID)
	otherClient, _ := coderdtest.CreateAnotherUser(t, ownerClient, owner.OrganizationID)
	admissiontest.Entitle(t, db, member.ID, managed.ID, "small")

	ctx := testutil.Context(t, testutil.WaitLong)

	workspace := coderdtest.CreateWorkspace(t, memberClient, managed.ID, func(req *codersdk.CreateWorkspaceRequest) {
		req.RichParameterValues = admissiontest.Profile("small")
	})
	build := workspace.LatestBuild
	jobID := build.Job.ID

	requireWaiting := func(t *testing.T, admission *codersdk.WorkspaceBuildAdmission, reason string, position *int) {
		t.Helper()
		require.NotNil(t, admission)
		require.Equal(t, codersdk.WorkspaceBuildAdmissionStatusWaiting, admission.Status)
		require.Equal(t, reason, admission.Reason)
		require.Equal(t, position, admission.QueuePosition)
		require.False(t, admission.QueuedAt.IsZero())
		require.Nil(t, admission.GrantedAt)
	}

	// The create response already carries the admission recorded in the
	// build transaction.
	requireWaiting(t, build.Admission, workspaceadmission.ReasonWaitingForCapacity, nil)

	// The controller reports a reason and a capacity queue position.
	position := 3
	_, err := db.UpdateWorkspaceBuildAdmissionDisplays(ctx, database.UpdateWorkspaceBuildAdmissionDisplaysParams{
		UpdatedAt:      dbtime.Now(),
		JobIds:         []uuid.UUID{jobID},
		Reasons:        []string{"2 workspaces ahead of you"},
		QueuePositions: []int32{3},
	})
	require.NoError(t, err)

	t.Run("Waiting", func(t *testing.T) { //nolint:paralleltest // Subtests share the admission lifecycle.
		got, err := memberClient.WorkspaceBuild(ctx, build.ID)
		require.NoError(t, err)
		requireWaiting(t, got.Admission, "2 workspaces ahead of you", &position)
		// A capacity wait never takes a provisioner queue position.
		require.Zero(t, got.Job.QueuePosition)

		gotWorkspace, err := memberClient.Workspace(ctx, workspace.ID)
		require.NoError(t, err)
		requireWaiting(t, gotWorkspace.LatestBuild.Admission, "2 workspaces ahead of you", &position)

		workspaces, err := memberClient.Workspaces(ctx, codersdk.WorkspaceFilter{})
		require.NoError(t, err)
		idx := slices.IndexFunc(workspaces.Workspaces, func(w codersdk.Workspace) bool { return w.ID == workspace.ID })
		require.GreaterOrEqual(t, idx, 0)
		requireWaiting(t, workspaces.Workspaces[idx].LatestBuild.Admission, "2 workspaces ahead of you", &position)

		builds, err := memberClient.WorkspaceBuilds(ctx, codersdk.WorkspaceBuildsRequest{WorkspaceID: workspace.ID})
		require.NoError(t, err)
		require.Len(t, builds, 1)
		requireWaiting(t, builds[0].Admission, "2 workspaces ahead of you", &position)

		byNumber, err := memberClient.WorkspaceBuildByUsernameAndWorkspaceNameAndBuildNumber(ctx, member.Username, workspace.Name, "1")
		require.NoError(t, err)
		requireWaiting(t, byNumber.Admission, "2 workspaces ahead of you", &position)

		// The JSON exposes only the user facing fields, never the
		// reservation, generation or operation of the controller.
		res, err := memberClient.Request(ctx, http.MethodGet, "/api/v2/workspacebuilds/"+build.ID.String(), nil)
		require.NoError(t, err)
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)
		var raw struct {
			Admission map[string]json.RawMessage `json:"admission"`
		}
		require.NoError(t, json.NewDecoder(res.Body).Decode(&raw))
		var keys []string
		for key := range raw.Admission {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		require.Equal(t, []string{"queue_position", "queued_at", "reason", "status"}, keys)
	})

	t.Run("OtherMember", func(t *testing.T) { //nolint:paralleltest // Subtests share the admission lifecycle.
		// The admission never widens who may read a build.
		_, err := otherClient.WorkspaceBuild(ctx, build.ID)
		require.Error(t, err)
		var sdkErr *codersdk.Error
		require.ErrorAs(t, err, &sdkErr)
		require.Equal(t, http.StatusNotFound, sdkErr.StatusCode())
	})

	t.Run("Granted", func(t *testing.T) { //nolint:paralleltest // Subtests share the admission lifecycle.
		admissiontest.Grant(t, db, nil, jobID)
		got, err := memberClient.WorkspaceBuild(ctx, build.ID)
		require.NoError(t, err)
		require.NotNil(t, got.Admission)
		require.Equal(t, codersdk.WorkspaceBuildAdmissionStatusGranted, got.Admission.Status)
		require.NotNil(t, got.Admission.GrantedAt)
		require.Nil(t, got.Admission.QueuePosition)
	})

	t.Run("CreateBuild", func(t *testing.T) { //nolint:paralleltest // Subtests share the admission lifecycle.
		// Templates only let owners cancel builds by default.
		err := ownerClient.CancelWorkspaceBuild(ctx, build.ID, codersdk.CancelWorkspaceBuildParams{})
		require.NoError(t, err)

		next, err := memberClient.CreateWorkspaceBuild(ctx, workspace.ID, codersdk.CreateWorkspaceBuildRequest{
			Transition:          codersdk.WorkspaceTransitionStart,
			RichParameterValues: admissiontest.Profile("small"),
		})
		require.NoError(t, err)
		requireWaiting(t, next.Admission, workspaceadmission.ReasonWaitingForCapacity, nil)

		builds, err := memberClient.WorkspaceBuilds(ctx, codersdk.WorkspaceBuildsRequest{WorkspaceID: workspace.ID})
		require.NoError(t, err)
		require.Len(t, builds, 2)
		for _, b := range builds {
			require.NotNil(t, b.Admission, "build %d", b.BuildNumber)
			switch b.ID {
			case build.ID:
				require.Equal(t, codersdk.WorkspaceBuildAdmissionStatusCanceled, b.Admission.Status)
				require.Equal(t, workspaceadmission.ReasonCanceled, b.Admission.Reason)
			case next.ID:
				require.Equal(t, codersdk.WorkspaceBuildAdmissionStatusWaiting, b.Admission.Status)
			default:
				t.Fatalf("unexpected build %s", b.ID)
			}
		}
	})

	t.Run("NotManaged", func(t *testing.T) { //nolint:paralleltest // Subtests share the admission lifecycle.
		plainWorkspace := coderdtest.CreateWorkspace(t, memberClient, plain.ID)
		require.Nil(t, plainWorkspace.LatestBuild.Admission)

		got, err := memberClient.WorkspaceBuild(ctx, plainWorkspace.LatestBuild.ID)
		require.NoError(t, err)
		require.Nil(t, got.Admission)

		res, err := memberClient.Request(ctx, http.MethodGet, "/api/v2/workspacebuilds/"+plainWorkspace.LatestBuild.ID.String(), nil)
		require.NoError(t, err)
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)
		var raw map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(res.Body).Decode(&raw))
		require.NotContains(t, raw, "admission")
	})
}
