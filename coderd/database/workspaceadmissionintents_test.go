package database_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbfake"
	"github.com/coder/coder/v2/coderd/database/dbgen"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/testutil"
)

// managedWorkspace creates a workspace whose latest build is a succeeded
// START with one agent, on a template managed by workspace admission.
func managedWorkspace(t *testing.T, db database.Store) dbfake.WorkspaceResponse {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	org := dbgen.Organization(t, db, database.Organization{})
	owner := dbgen.User(t, db, database.User{})
	resp := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
		OrganizationID: org.ID,
		OwnerID:        owner.ID,
	}).WithAgent().Do()
	_, err := db.UpsertWorkspaceAdmissionTemplate(ctx, database.UpsertWorkspaceAdmissionTemplateParams{
		ProfileParameter: admissionProfileParameter,
		Now:              dbtime.Now(),
		TemplateID:       resp.Workspace.TemplateID,
	})
	require.NoError(t, err)
	return resp
}

func upsertWakeIntent(t *testing.T, db database.Store, ws database.WorkspaceTable, agentID uuid.UUID, first, last time.Time, count int32) database.WorkspaceAdmissionWakeIntent {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	intent, err := db.UpsertWorkspaceAdmissionWakeIntent(ctx, database.UpsertWorkspaceAdmissionWakeIntentParams{
		WorkspaceID:      ws.ID,
		OrganizationID:   ws.OrganizationID,
		AgentID:          agentID,
		Source:           "tailnet",
		RequestedBy:      uuid.NullUUID{UUID: ws.OwnerID, Valid: true},
		FirstRequestedAt: first,
		LastRequestedAt:  last,
		RequestCount:     count,
	})
	require.NoError(t, err)
	return intent
}

func pendingWakeIntents(t *testing.T, db database.Store) map[uuid.UUID]database.WorkspaceAdmissionWakeIntent {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	rows, err := db.GetWorkspaceAdmissionWakeIntents(ctx, database.GetWorkspaceAdmissionWakeIntentsParams{
		PendingOnly: true,
		LimitCount:  1000,
	})
	require.NoError(t, err)
	out := make(map[uuid.UUID]database.WorkspaceAdmissionWakeIntent, len(rows))
	for _, row := range rows {
		out[row.WorkspaceID] = row
	}
	return out
}

func TestWorkspaceAdmissionWakeIntents(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ws := managedWorkspace(t, db)
	agentID := ws.Agents[0].ID
	ctx := testutil.Context(t, testutil.WaitLong)

	before, err := db.GetWorkspaceBuildAdmissionsWatermark(ctx)
	require.NoError(t, err)

	t0 := dbtime.Now().Add(time.Second)
	first := upsertWakeIntent(t, db, ws.Workspace, agentID, t0, t0, 1)
	require.Equal(t, int32(1), first.RequestCount)
	require.True(t, first.FirstRequestedAt.Equal(t0))
	require.True(t, first.LastRequestedAt.Equal(t0))
	require.False(t, first.AckedAt.Valid)
	require.Contains(t, pendingWakeIntents(t, db), ws.Workspace.ID)

	// The watermark moves with intents so the long poll wakes on them.
	afterFirst, err := db.GetWorkspaceBuildAdmissionsWatermark(ctx)
	require.NoError(t, err)
	require.True(t, afterFirst.After(before), "watermark must move after an intent")

	// Repeats extend the pending intent.
	t1 := t0.Add(500 * time.Millisecond)
	second := upsertWakeIntent(t, db, ws.Workspace, agentID, t1, t1, 2)
	require.Equal(t, int32(3), second.RequestCount)
	require.True(t, second.FirstRequestedAt.Equal(t0))
	require.True(t, second.LastRequestedAt.Equal(t1))

	// A request stamped earlier than the stored one still moves the latest
	// request forward, so acknowledgements can tell requests apart.
	third := upsertWakeIntent(t, db, ws.Workspace, agentID, t0, t0, 1)
	require.True(t, third.LastRequestedAt.After(second.LastRequestedAt))
	require.Equal(t, int32(4), third.RequestCount)

	// An acknowledgement of a request the controller did not see is ignored.
	rows, err := db.AckWorkspaceAdmissionWakeIntent(ctx, database.AckWorkspaceAdmissionWakeIntentParams{
		Now:             dbtime.Now(),
		PowerEpoch:      3,
		Result:          "woken",
		WorkspaceID:     ws.Workspace.ID,
		LastRequestedAt: second.LastRequestedAt,
	})
	require.NoError(t, err)
	require.Zero(t, rows)
	require.Contains(t, pendingWakeIntents(t, db), ws.Workspace.ID)

	ackAt := dbtime.Now().Add(2 * time.Second)
	rows, err = db.AckWorkspaceAdmissionWakeIntent(ctx, database.AckWorkspaceAdmissionWakeIntentParams{
		Now:             ackAt,
		PowerEpoch:      3,
		Result:          "woken",
		WorkspaceID:     ws.Workspace.ID,
		LastRequestedAt: third.LastRequestedAt,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	require.NotContains(t, pendingWakeIntents(t, db), ws.Workspace.ID)

	all, err := db.GetWorkspaceAdmissionWakeIntents(ctx, database.GetWorkspaceAdmissionWakeIntentsParams{LimitCount: 1000})
	require.NoError(t, err)
	var acked database.WorkspaceAdmissionWakeIntent
	for _, row := range all {
		if row.WorkspaceID == ws.Workspace.ID {
			acked = row
		}
	}
	require.True(t, acked.AckedAt.Valid)
	require.True(t, acked.AckedAt.Time.Equal(ackAt))
	require.Equal(t, int64(3), acked.AckedPowerEpoch.Int64)
	require.Equal(t, "woken", acked.AckedResult.String)

	// Repeating the acknowledgement keeps the first acknowledgement.
	rows, err = db.AckWorkspaceAdmissionWakeIntent(ctx, database.AckWorkspaceAdmissionWakeIntentParams{
		Now:             ackAt.Add(time.Minute),
		PowerEpoch:      3,
		Result:          "woken",
		WorkspaceID:     ws.Workspace.ID,
		LastRequestedAt: third.LastRequestedAt,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	watermarkAfterAck, err := db.GetWorkspaceBuildAdmissionsWatermark(ctx)
	require.NoError(t, err)
	require.False(t, watermarkAfterAck.Before(ackAt))

	// A request after the acknowledgement starts a new pending intent.
	t2 := ackAt.Add(time.Second)
	fresh := upsertWakeIntent(t, db, ws.Workspace, agentID, t2, t2, 1)
	require.Equal(t, int32(1), fresh.RequestCount)
	require.True(t, fresh.FirstRequestedAt.Equal(t2))
	require.False(t, fresh.AckedAt.Valid)
	require.False(t, fresh.AckedPowerEpoch.Valid)
	require.False(t, fresh.AckedResult.Valid)
	require.Contains(t, pendingWakeIntents(t, db), ws.Workspace.ID)
}

// TestWorkspaceAdmissionWakeIntentAckRace checks that a request recorded
// while the controller wakes the workspace is not lost when the controller
// acknowledges the request it saw before.
func TestWorkspaceAdmissionWakeIntentAckRace(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ws := managedWorkspace(t, db)
	ctx := testutil.Context(t, testutil.WaitLong)

	now := dbtime.Now()
	seen := upsertWakeIntent(t, db, ws.Workspace, ws.Agents[0].ID, now, now, 1)
	// Recorded with the same timestamp, as a coarse clock would.
	upsertWakeIntent(t, db, ws.Workspace, ws.Agents[0].ID, now, now, 1)

	rows, err := db.AckWorkspaceAdmissionWakeIntent(ctx, database.AckWorkspaceAdmissionWakeIntentParams{
		Now:             dbtime.Now(),
		PowerEpoch:      1,
		Result:          "woken",
		WorkspaceID:     ws.Workspace.ID,
		LastRequestedAt: seen.LastRequestedAt,
	})
	require.NoError(t, err)
	require.Zero(t, rows)
	pending := pendingWakeIntents(t, db)
	require.Contains(t, pending, ws.Workspace.ID)
	require.Equal(t, int32(2), pending[ws.Workspace.ID].RequestCount)
}

func TestWorkspaceAdmissionWakeIntentListOrderAndLimit(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	base := dbtime.Now()
	var ids []uuid.UUID
	for i := range 3 {
		ws := managedWorkspace(t, db)
		at := base.Add(time.Duration(3-i) * time.Second)
		upsertWakeIntent(t, db, ws.Workspace, ws.Agents[0].ID, at, at, 1)
		ids = append([]uuid.UUID{ws.Workspace.ID}, ids...)
	}
	rows, err := db.GetWorkspaceAdmissionWakeIntents(ctx, database.GetWorkspaceAdmissionWakeIntentsParams{
		PendingOnly: true,
		LimitCount:  2,
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, ids[0], rows[0].WorkspaceID)
	require.Equal(t, ids[1], rows[1].WorkspaceID)
}

func TestWorkspaceAdmissionWakeIntentConstraints(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ws := managedWorkspace(t, db)
	ctx := testutil.Context(t, testutil.WaitLong)
	now := dbtime.Now()

	_, err := db.UpsertWorkspaceAdmissionWakeIntent(ctx, database.UpsertWorkspaceAdmissionWakeIntentParams{
		WorkspaceID:      ws.Workspace.ID,
		OrganizationID:   ws.Workspace.OrganizationID,
		AgentID:          ws.Agents[0].ID,
		Source:           "desktop",
		FirstRequestedAt: now,
		LastRequestedAt:  now,
		RequestCount:     1,
	})
	require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionWakeIntentsSourceCheck), "got %v", err)

	for _, source := range []string{"tailnet", "cli", "pty", "app"} {
		_, err := db.UpsertWorkspaceAdmissionWakeIntent(ctx, database.UpsertWorkspaceAdmissionWakeIntentParams{
			WorkspaceID:      ws.Workspace.ID,
			OrganizationID:   ws.Workspace.OrganizationID,
			AgentID:          ws.Agents[0].ID,
			Source:           source,
			FirstRequestedAt: now,
			LastRequestedAt:  now,
			RequestCount:     1,
		})
		require.NoError(t, err, source)
	}

	intent := pendingWakeIntents(t, db)[ws.Workspace.ID]
	_, err = db.AckWorkspaceAdmissionWakeIntent(ctx, database.AckWorkspaceAdmissionWakeIntentParams{
		Now:             now,
		Result:          "sleeping",
		WorkspaceID:     ws.Workspace.ID,
		LastRequestedAt: intent.LastRequestedAt,
	})
	require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionWakeIntentsAckedResultCheck), "got %v", err)
	for _, result := range []string{"woken", "already_running", "failed", "ignored"} {
		_, err := db.AckWorkspaceAdmissionWakeIntent(ctx, database.AckWorkspaceAdmissionWakeIntentParams{
			Now:             now,
			Result:          result,
			WorkspaceID:     ws.Workspace.ID,
			LastRequestedAt: intent.LastRequestedAt,
		})
		require.NoError(t, err, result)
	}
}

func TestWorkspaceAdmissionKeepaliveIntents(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ws := managedWorkspace(t, db)
	ctx := testutil.Context(t, testutil.WaitLong)

	pending := func() map[uuid.UUID]database.WorkspaceAdmissionKeepaliveIntent {
		rows, err := db.GetWorkspaceAdmissionKeepaliveIntents(ctx, database.GetWorkspaceAdmissionKeepaliveIntentsParams{
			PendingOnly: true,
			LimitCount:  1000,
		})
		require.NoError(t, err)
		out := make(map[uuid.UUID]database.WorkspaceAdmissionKeepaliveIntent, len(rows))
		for _, row := range rows {
			out[row.WorkspaceID] = row
		}
		return out
	}
	upsert := func(at time.Time, forSeconds int32) (database.WorkspaceAdmissionKeepaliveIntent, error) {
		return db.UpsertWorkspaceAdmissionKeepaliveIntent(ctx, database.UpsertWorkspaceAdmissionKeepaliveIntentParams{
			WorkspaceID:    ws.Workspace.ID,
			OrganizationID: ws.Workspace.OrganizationID,
			RequestedBy:    ws.Workspace.OwnerID,
			RequestedAt:    at,
			ForSeconds:     forSeconds,
		})
	}

	before, err := db.GetWorkspaceBuildAdmissionsWatermark(ctx)
	require.NoError(t, err)

	now := dbtime.Now().Add(time.Second)
	first, err := upsert(now, 7200)
	require.NoError(t, err)
	require.Contains(t, pending(), ws.Workspace.ID)
	after, err := db.GetWorkspaceBuildAdmissionsWatermark(ctx)
	require.NoError(t, err)
	require.True(t, after.After(before), "watermark must move after a keepalive intent")

	// A newer press replaces the request and moves requested_at forward even
	// with the same timestamp.
	second, err := upsert(now, 3600)
	require.NoError(t, err)
	require.True(t, second.RequestedAt.After(first.RequestedAt))
	require.Equal(t, int32(3600), second.ForSeconds)

	rows, err := db.AckWorkspaceAdmissionKeepaliveIntent(ctx, database.AckWorkspaceAdmissionKeepaliveIntentParams{
		Now:         dbtime.Now(),
		Until:       sql.NullTime{Time: now.Add(time.Hour), Valid: true},
		Result:      "applied",
		WorkspaceID: ws.Workspace.ID,
		RequestedAt: first.RequestedAt,
	})
	require.NoError(t, err)
	require.Zero(t, rows, "an acknowledgement of an older request must not match")
	require.Contains(t, pending(), ws.Workspace.ID)

	until := now.Add(time.Hour)
	rows, err = db.AckWorkspaceAdmissionKeepaliveIntent(ctx, database.AckWorkspaceAdmissionKeepaliveIntentParams{
		Now:         dbtime.Now(),
		Until:       sql.NullTime{Time: until, Valid: true},
		Result:      "applied",
		WorkspaceID: ws.Workspace.ID,
		RequestedAt: second.RequestedAt,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	require.NotContains(t, pending(), ws.Workspace.ID)

	all, err := db.GetWorkspaceAdmissionKeepaliveIntents(ctx, database.GetWorkspaceAdmissionKeepaliveIntentsParams{LimitCount: 1000})
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.True(t, all[0].AckedUntil.Time.Equal(until))
	require.Equal(t, "applied", all[0].AckedResult.String)

	// A denial carries no end time.
	third, err := upsert(dbtime.Now(), 7200)
	require.NoError(t, err)
	require.False(t, third.AckedAt.Valid)
	require.False(t, third.AckedUntil.Valid)
	rows, err = db.AckWorkspaceAdmissionKeepaliveIntent(ctx, database.AckWorkspaceAdmissionKeepaliveIntentParams{
		Now:         dbtime.Now(),
		Result:      "denied",
		WorkspaceID: ws.Workspace.ID,
		RequestedAt: third.RequestedAt,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	_, err = upsert(dbtime.Now(), 0)
	require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionKeepaliveIntentsForSecondsCheck), "got %v", err)
	_, err = upsert(dbtime.Now(), 43201)
	require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionKeepaliveIntentsForSecondsCheck), "got %v", err)
	latest, err := upsert(dbtime.Now(), 43200)
	require.NoError(t, err)
	_, err = db.AckWorkspaceAdmissionKeepaliveIntent(ctx, database.AckWorkspaceAdmissionKeepaliveIntentParams{
		Now:         dbtime.Now(),
		Result:      "extended",
		WorkspaceID: ws.Workspace.ID,
		RequestedAt: latest.RequestedAt,
	})
	require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionKeepaliveIntentsAckedResultCheck), "got %v", err)
}

func TestWorkspaceAdmissionWakeTarget(t *testing.T) {
	t.Parallel()

	db, _, sqlDB := dbtestutil.NewDBWithSQLDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)

	ws := managedWorkspace(t, db)
	target, err := db.GetWorkspaceAdmissionWakeTargetByAgentID(ctx, ws.Agents[0].ID)
	require.NoError(t, err)
	require.Equal(t, ws.Workspace.ID, target.WorkspaceID)
	require.Equal(t, ws.Workspace.OrganizationID, target.OrganizationID)
	require.Equal(t, ws.Workspace.OwnerID, target.OwnerID)
	require.True(t, target.Managed)
	require.True(t, target.AgentInLatestBuild)
	require.False(t, target.WorkspaceDeleted)
	require.Equal(t, database.WorkspaceTransitionStart, target.LatestBuildTransition)
	require.Equal(t, database.ProvisionerJobStatusSucceeded, target.LatestJobStatus)
	require.False(t, target.PowerState.Valid)
	require.Equal(t, ws.Agents[0].ID, target.WorkspaceAgent.ID)

	_, err = sqlDB.ExecContext(ctx, `INSERT INTO workspace_admission_workspace_states
		(workspace_id, organization_id, power_state, power_epoch, generation, since, updated_at)
		VALUES ($1, $2, 'hibernated', 4, 1, NOW(), NOW())`, ws.Workspace.ID, ws.Workspace.OrganizationID)
	require.NoError(t, err)
	target, err = db.GetWorkspaceAdmissionWakeTargetByAgentID(ctx, ws.Agents[0].ID)
	require.NoError(t, err)
	require.Equal(t, "hibernated", target.PowerState.String)

	// A STOP build makes the old agent stale.
	dbfake.WorkspaceBuild(t, db, ws.Workspace).Seed(database.WorkspaceBuild{
		Transition:  database.WorkspaceTransitionStop,
		BuildNumber: 2,
	}).Do()
	target, err = db.GetWorkspaceAdmissionWakeTargetByAgentID(ctx, ws.Agents[0].ID)
	require.NoError(t, err)
	require.False(t, target.AgentInLatestBuild)
	require.Equal(t, database.WorkspaceTransitionStop, target.LatestBuildTransition)

	// Unmanaged templates are reported as such.
	org := dbgen.Organization(t, db, database.Organization{})
	owner := dbgen.User(t, db, database.User{})
	unmanaged := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
		OrganizationID: org.ID,
		OwnerID:        owner.ID,
	}).WithAgent().Do()
	target, err = db.GetWorkspaceAdmissionWakeTargetByAgentID(ctx, unmanaged.Agents[0].ID)
	require.NoError(t, err)
	require.False(t, target.Managed)

	_, err = db.GetWorkspaceAdmissionWakeTargetByAgentID(ctx, uuid.New())
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestWorkspaceAdmissionWorkspaceAgents(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)

	ws := managedWorkspace(t, db)
	second := managedWorkspace(t, db)
	// A sub-agent of the first workspace is not listed.
	dbgen.WorkspaceAgent(t, db, database.WorkspaceAgent{
		ResourceID: ws.Agents[0].ResourceID,
		ParentID:   uuid.NullUUID{UUID: ws.Agents[0].ID, Valid: true},
	})

	rows, err := db.GetWorkspaceAdmissionWorkspaceAgents(ctx, ws.Workspace.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, ws.Workspace.ID, rows[0].WorkspaceID)
	require.Equal(t, ws.Agents[0].ID, rows[0].WorkspaceAgent.ID)

	all, err := db.GetWorkspaceAdmissionWorkspaceAgents(ctx, uuid.Nil)
	require.NoError(t, err)
	seen := map[uuid.UUID]uuid.UUID{}
	for _, row := range all {
		seen[row.WorkspaceID] = row.WorkspaceAgent.ID
	}
	require.Equal(t, ws.Agents[0].ID, seen[ws.Workspace.ID])
	require.Equal(t, second.Agents[0].ID, seen[second.Workspace.ID])

	// After a STOP build the workspace has no agents.
	dbfake.WorkspaceBuild(t, db, second.Workspace).Seed(database.WorkspaceBuild{
		Transition:  database.WorkspaceTransitionStop,
		BuildNumber: 2,
	}).Do()
	rows, err = db.GetWorkspaceAdmissionWorkspaceAgents(ctx, second.Workspace.ID)
	require.NoError(t, err)
	require.Empty(t, rows)

	// Agents of unmanaged workspaces are not listed.
	org := dbgen.Organization(t, db, database.Organization{})
	owner := dbgen.User(t, db, database.User{})
	unmanaged := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
		OrganizationID: org.ID,
		OwnerID:        owner.ID,
	}).WithAgent().Do()
	rows, err = db.GetWorkspaceAdmissionWorkspaceAgents(ctx, unmanaged.Workspace.ID)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestWorkspaceAdmissionWorkspaceStateConstraints checks the power state,
// run and fact constraints that the controller state push relies on.
func TestWorkspaceAdmissionWorkspaceStateConstraints(t *testing.T) {
	t.Parallel()

	db, _, sqlDB := dbtestutil.NewDBWithSQLDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	ws := managedWorkspace(t, db)

	insert := func(powerState, runs, lastSample string) error {
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO workspace_admission_workspace_states
			(workspace_id, organization_id, power_state, power_epoch, generation, since, runs, last_sample, updated_at)
			VALUES ($1, $2, $3, 1, 1, NOW(), $4::jsonb, $5::jsonb, NOW())
			ON CONFLICT (workspace_id) DO UPDATE SET power_state = EXCLUDED.power_state, runs = EXCLUDED.runs, last_sample = EXCLUDED.last_sample`,
			ws.Workspace.ID, ws.Workspace.OrganizationID, powerState, runs, sql.NullString{String: lastSample, Valid: lastSample != ""})
		return err
	}
	run := func(startedBy, state string) string {
		return fmt.Sprintf(`{"id":%q,"harness":"codex","mode":"A","state":%q,"waiting_reason":"","started_by":%q,"grant_id":"","last_event_at":"2026-10-30T00:00:00Z"}`,
			uuid.NewString(), state, startedBy)
	}

	for _, state := range []string{"running", "reduced", "paused", "hibernated", "cold", "waking", "sleeping", "unknown"} {
		require.NoError(t, insert(state, "[]", ""), state)
	}
	for _, state := range []string{"degraded", "off", "transition", ""} {
		err := insert(state, "[]", "")
		require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionWorkspaceStatesPowerStateCheck), "%q: %v", state, err)
	}

	for _, startedBy := range []string{"employee", "grant"} {
		for _, state := range []string{"running", "waiting_model", "waiting_human", "idle", "done", "failed"} {
			require.NoError(t, insert("running", "["+run(startedBy, state)+"]", ""), "%s/%s", startedBy, state)
		}
	}
	for name, runs := range map[string]string{
		"started_by agent":   "[" + run("agent", "running") + "]",
		"state waiting":      "[" + run("employee", "waiting") + "]",
		"missing started_by": `[{"id":"x","state":"running"}]`,
		"missing state":      `[{"id":"x","started_by":"employee"}]`,
		"not an array":       `{"id":"x"}`,
		"too many runs":      "[" + strings.TrimSuffix(strings.Repeat(run("employee", "idle")+",", 21), ",") + "]",
	} {
		err := insert("running", runs, "")
		require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionWorkspaceStatesRunsCheck), "%s: %v", name, err)
	}
	require.NoError(t, insert("running", "["+strings.TrimSuffix(strings.Repeat(run("grant", "idle")+",", 20), ",")+"]", ""))

	require.NoError(t, insert("running", "[]", `{"memory_mib":512}`))
	err := insert("running", "[]", fmt.Sprintf(`{"pad":%q}`, strings.Repeat("x", 4096)))
	require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionWorkspaceStatesLastSampleCheck), "got %v", err)
}
