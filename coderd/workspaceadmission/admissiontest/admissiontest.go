// Package admissiontest provides helpers for tests of workspace admission.
package admissiontest

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/database/provisionerjobs"
	"github.com/coder/coder/v2/coderd/database/pubsub"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/provisioner/echo"
	"github.com/coder/coder/v2/provisionersdk"
	"github.com/coder/coder/v2/provisionersdk/proto"
	"github.com/coder/coder/v2/testutil"
)

// ProfileParameter is the build parameter that selects the profile in
// templates created from ProfileResponses.
const ProfileParameter = "profile"

// ProfileResponses returns echo responses for a template with a mutable
// "profile" parameter that offers "small" and "large".
func ProfileResponses() *echo.Responses {
	return &echo.Responses{
		Parse: echo.ParseComplete,
		ProvisionGraph: []*proto.Response{{
			Type: &proto.Response_Graph{
				Graph: &proto.GraphComplete{
					Parameters: []*proto.RichParameter{{
						Name:         ProfileParameter,
						Type:         "string",
						DefaultValue: "small",
						Mutable:      true,
						FormType:     proto.ParameterFormType_RADIO,
						Options: []*proto.RichParameterOption{
							{Name: "small", Value: "small"},
							{Name: "large", Value: "large"},
						},
					}},
				},
			},
		}},
		ProvisionApply: echo.ApplyComplete,
	}
}

// Profile returns build parameters that select profile.
func Profile(profile string) []codersdk.WorkspaceBuildParameter {
	return []codersdk.WorkspaceBuildParameter{{Name: ProfileParameter, Value: profile}}
}

// Manage marks a template as managed by workspace admission. db must not be
// wrapped by dbauthz.
func Manage(t testing.TB, db database.Store, templateID uuid.UUID, profileParameter string) {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitShort)
	_, err := db.UpsertWorkspaceAdmissionTemplate(ctx, database.UpsertWorkspaceAdmissionTemplateParams{
		ProfileParameter: profileParameter,
		Now:              dbtime.Now(),
		TemplateID:       templateID,
	})
	require.NoError(t, err)
}

// Entitle lets userID start the given profiles on a managed template.
func Entitle(t testing.TB, db database.Store, userID, templateID uuid.UUID, profiles ...string) {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitShort)
	_, err := db.InsertWorkspaceAdmissionEntitlement(ctx, database.InsertWorkspaceAdmissionEntitlementParams{
		UserID:          userID,
		AllowedProfiles: profiles,
		UpdatedAt:       dbtime.Now(),
		TemplateID:      templateID,
	})
	require.NoError(t, err)
}

// Admission returns the admission of a build job.
func Admission(t testing.TB, db database.Store, jobID uuid.UUID) database.WorkspaceBuildAdmission {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitShort)
	admission, err := db.GetWorkspaceBuildAdmissionByJobID(ctx, jobID)
	require.NoError(t, err)
	return admission
}

// RequireNoAdmission asserts that a build job has no admission.
func RequireNoAdmission(t testing.TB, db database.Store, jobID uuid.UUID) {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitShort)
	_, err := db.GetWorkspaceBuildAdmissionByJobID(ctx, jobID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// Grant grants a waiting admission directly in the database and wakes
// provisioners, the way the admission service does.
func Grant(t testing.TB, db database.Store, ps pubsub.Pubsub, jobID uuid.UUID) database.WorkspaceBuildAdmission {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitShort)
	now := dbtime.Now()
	admission, err := db.GrantWorkspaceBuildAdmission(ctx, database.GrantWorkspaceBuildAdmissionParams{
		ReservationID:  uuid.New(),
		Generation:     1,
		OperationID:    uuid.New(),
		GrantExpiresAt: now.Add(time.Hour),
		Now:            now,
		JobID:          jobID,
	})
	require.NoError(t, err)
	if ps != nil {
		job, err := db.GetProvisionerJobByID(ctx, jobID)
		require.NoError(t, err)
		require.NoError(t, provisionerjobs.PostJob(ps, job))
	}
	return admission
}

// Reject rejects a pending admission and fails its job the way the admission
// service does, then ends the job's log stream so watchers finish.
func Reject(t testing.TB, db database.Store, ps pubsub.Pubsub, jobID uuid.UUID, reason string) {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitShort)
	now := dbtime.Now()
	_, err := db.UpdatePendingWorkspaceBuildAdmissionStatus(ctx, database.UpdatePendingWorkspaceBuildAdmissionStatusParams{
		Status: database.WorkspaceAdmissionStatusRejected,
		Reason: reason,
		Now:    now,
		JobID:  jobID,
	})
	require.NoError(t, err)
	err = db.UpdateProvisionerJobWithCompleteWithStartedAtByID(ctx, database.UpdateProvisionerJobWithCompleteWithStartedAtByIDParams{
		ID:          jobID,
		UpdatedAt:   now,
		CompletedAt: sql.NullTime{Time: now, Valid: true},
		Error:       sql.NullString{String: "admission rejected: " + reason, Valid: true},
		ErrorCode:   sql.NullString{},
		StartedAt:   sql.NullTime{Time: now, Valid: true},
	})
	require.NoError(t, err)
	if ps != nil {
		msg, err := json.Marshal(provisionersdk.ProvisionerJobLogsNotifyMessage{EndOfLogs: true})
		require.NoError(t, err)
		require.NoError(t, ps.Publish(provisionersdk.ProvisionerJobLogsNotifyChannel(jobID), msg))
	}
}

// Acquire acquires any pending job of the organization that an untagged
// provisioner could run, as a provisioner would.
func Acquire(ctx context.Context, db database.Store, orgID uuid.UUID) (database.ProvisionerJob, error) {
	return db.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
		OrganizationID:  orgID,
		StartedAt:       sql.NullTime{Time: dbtime.Now(), Valid: true},
		WorkerID:        uuid.NullUUID{UUID: uuid.New(), Valid: true},
		Types:           []database.ProvisionerType{database.ProvisionerTypeEcho},
		ProvisionerTags: []byte(`{"scope":"organization","owner":""}`),
	})
}
