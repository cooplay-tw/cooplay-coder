package database_test

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/testutil"
)

func workspaceStateParams(ws database.WorkspaceTable, state string, epoch int64) database.UpsertWorkspaceAdmissionWorkspaceStateParams {
	now := dbtime.Now()
	return database.UpsertWorkspaceAdmissionWorkspaceStateParams{
		WorkspaceID:    ws.ID,
		OrganizationID: ws.OrganizationID,
		PowerState:     state,
		PowerEpoch:     epoch,
		Generation:     1,
		Since:          now,
		Reason:         "idle",
		Runs:           json.RawMessage(`[]`),
		UpdatedAt:      now,
	}
}

// TestWorkspaceAdmissionWorkspaceStates checks the compare-and-set on the
// power epoch, also across the controller reporting the workspace as off,
// and that readers treat an off state as absent.
func TestWorkspaceAdmissionWorkspaceStates(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	ws := managedWorkspace(t, db)
	agentID := ws.Agents[0].ID

	stored, err := db.UpsertWorkspaceAdmissionWorkspaceState(ctx, workspaceStateParams(ws.Workspace, "hibernated", 5))
	require.NoError(t, err)
	require.Equal(t, "hibernated", stored.PowerState)
	require.EqualValues(t, 5, stored.PowerEpoch)
	require.False(t, stored.OffAt.Valid)
	updatedAt := stored.UpdatedAt

	// An older epoch changes nothing.
	_, err = db.UpsertWorkspaceAdmissionWorkspaceState(ctx, workspaceStateParams(ws.Workspace, "running", 4))
	require.ErrorIs(t, err, sql.ErrNoRows)
	stored, err = db.GetWorkspaceAdmissionWorkspaceStateByWorkspaceID(ctx, ws.Workspace.ID)
	require.NoError(t, err)
	require.Equal(t, "hibernated", stored.PowerState)

	// An equal epoch replaces the state, and updated_at strictly increases
	// even when the pushed time does not.
	params := workspaceStateParams(ws.Workspace, "running", 5)
	params.UpdatedAt = updatedAt
	params.Runs = json.RawMessage(`[{"id":"r-1","state":"running","started_by":"grant"}]`)
	params.Git = pqtype.NullRawMessage{RawMessage: json.RawMessage(`{"branch":"main"}`), Valid: true}
	stored, err = db.UpsertWorkspaceAdmissionWorkspaceState(ctx, params)
	require.NoError(t, err)
	require.Equal(t, "running", stored.PowerState)
	require.True(t, stored.UpdatedAt.After(updatedAt))

	target, err := db.GetWorkspaceAdmissionWakeTargetByAgentID(ctx, agentID)
	require.NoError(t, err)
	require.Equal(t, "running", target.PowerState.String)

	rows, err := db.GetWorkspaceAdmissionCooplayByWorkspaceIDs(ctx, []uuid.UUID{ws.Workspace.ID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "running", rows[0].PowerState.String)
	require.JSONEq(t, `[{"id":"r-1","state":"running","started_by":"grant"}]`, string(rows[0].Runs.RawMessage))
	require.JSONEq(t, `{"branch":"main"}`, string(rows[0].Git.RawMessage))
	require.False(t, rows[0].CreateSource.Valid)

	// Off keeps the epoch, so an older state is still rejected, and readers
	// see no state.
	now := dbtime.Now()
	cleared, err := db.ClearWorkspaceAdmissionWorkspaceState(ctx, database.ClearWorkspaceAdmissionWorkspaceStateParams{
		WorkspaceID: ws.Workspace.ID,
		Now:         now,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, cleared)
	cleared, err = db.ClearWorkspaceAdmissionWorkspaceState(ctx, database.ClearWorkspaceAdmissionWorkspaceStateParams{
		WorkspaceID: ws.Workspace.ID,
		Now:         now.Add(time.Second),
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, cleared, "clearing an off state changes nothing")
	stored, err = db.GetWorkspaceAdmissionWorkspaceStateByWorkspaceID(ctx, ws.Workspace.ID)
	require.NoError(t, err)
	require.True(t, stored.OffAt.Valid)
	require.EqualValues(t, 5, stored.PowerEpoch)

	target, err = db.GetWorkspaceAdmissionWakeTargetByAgentID(ctx, agentID)
	require.NoError(t, err)
	require.False(t, target.PowerState.Valid)
	rows, err = db.GetWorkspaceAdmissionCooplayByWorkspaceIDs(ctx, []uuid.UUID{ws.Workspace.ID})
	require.NoError(t, err)
	require.Empty(t, rows)

	_, err = db.UpsertWorkspaceAdmissionWorkspaceState(ctx, workspaceStateParams(ws.Workspace, "running", 4))
	require.ErrorIs(t, err, sql.ErrNoRows)

	// The same epoch, as after a START from off, brings the state back.
	stored, err = db.UpsertWorkspaceAdmissionWorkspaceState(ctx, workspaceStateParams(ws.Workspace, "waking", 5))
	require.NoError(t, err)
	require.False(t, stored.OffAt.Valid)
	require.Equal(t, "waking", stored.PowerState)
	rows, err = db.GetWorkspaceAdmissionCooplayByWorkspaceIDs(ctx, []uuid.UUID{ws.Workspace.ID})
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// A workspace without a state is not cleared.
	other := managedWorkspace(t, db)
	cleared, err = db.ClearWorkspaceAdmissionWorkspaceState(ctx, database.ClearWorkspaceAdmissionWorkspaceStateParams{
		WorkspaceID: other.Workspace.ID,
		Now:         dbtime.Now(),
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, cleared)
}

// TestWorkspaceAdmissionCooplaySource checks that the create source and its
// grant are read for workspaces without a reported power state.
func TestWorkspaceAdmissionCooplaySource(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	ws := managedWorkspace(t, db)
	plain := managedWorkspace(t, db)

	expiresAt := dbtime.Now().Add(time.Hour)
	_, err := db.UpsertWorkspaceAdmissionGrant(ctx, database.UpsertWorkspaceAdmissionGrantParams{
		ID:             "g-source",
		OwnerID:        ws.Workspace.OwnerID,
		OrganizationID: ws.Workspace.OrganizationID,
		TemplateIDs:    []uuid.UUID{},
		MaxWorkspaces:  1,
		ExpiresAt:      expiresAt,
		Now:            dbtime.Now(),
	})
	require.NoError(t, err)
	source := json.RawMessage(`{"kind":"agent_grant","grant_id":"g-source","run_id":"r-source","harness":"claude-code","initiator_workspace_id":"","clone_of_workspace_id":""}`)
	_, err = db.InsertWorkspaceAdmissionCreateOperation(ctx, database.InsertWorkspaceAdmissionCreateOperationParams{
		OperationID:      uuid.New(),
		WorkspaceID:      ws.Workspace.ID,
		WorkspaceBuildID: ws.Build.ID,
		JobID:            ws.Build.JobID,
		OrganizationID:   ws.Workspace.OrganizationID,
		OwnerID:          ws.Workspace.OwnerID,
		GrantID:          sql.NullString{String: "g-source", Valid: true},
		RunID:            sql.NullString{String: "r-source", Valid: true},
		Source:           source,
		CreatedAt:        dbtime.Now(),
	})
	require.NoError(t, err)

	rows, err := db.GetWorkspaceAdmissionCooplayByWorkspaceIDs(ctx, []uuid.UUID{ws.Workspace.ID, plain.Workspace.ID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, ws.Workspace.ID, rows[0].WorkspaceID)
	require.False(t, rows[0].PowerState.Valid)
	require.False(t, rows[0].Runs.Valid)
	require.JSONEq(t, string(source), string(rows[0].CreateSource.RawMessage))
	require.True(t, rows[0].GrantExpiresAt.Time.Equal(expiresAt))
	require.False(t, rows[0].GrantRevokedAt.Valid)
}
