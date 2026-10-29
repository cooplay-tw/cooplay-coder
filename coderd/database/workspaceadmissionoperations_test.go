package database_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/testutil"
)

func upsertGrant(t *testing.T, db database.Store, arg database.UpsertWorkspaceAdmissionGrantParams) database.WorkspaceAdmissionGrant {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	if arg.Now.IsZero() {
		arg.Now = dbtime.Now()
	}
	if arg.ExpiresAt.IsZero() {
		arg.ExpiresAt = arg.Now.Add(time.Hour)
	}
	grant, err := db.UpsertWorkspaceAdmissionGrant(ctx, arg)
	require.NoError(t, err)
	return grant
}

func TestWorkspaceAdmissionGrants(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ws := managedWorkspace(t, db).Workspace
	ctx := testutil.Context(t, testutil.WaitLong)

	grant := upsertGrant(t, db, database.UpsertWorkspaceAdmissionGrantParams{
		ID:             "g-01jbx5870000000000000000aa",
		OwnerID:        ws.OwnerID,
		OrganizationID: ws.OrganizationID,
		TemplateIDs:    []uuid.UUID{ws.TemplateID},
		NamePrefix:     "agent-",
		MaxWorkspaces:  2,
	})
	require.False(t, grant.RevokedAt.Valid)
	require.Equal(t, []uuid.UUID{ws.TemplateID}, grant.TemplateIDs)

	// A later mirror replaces the scope.
	grant = upsertGrant(t, db, database.UpsertWorkspaceAdmissionGrantParams{
		ID:             grant.ID,
		OwnerID:        ws.OwnerID,
		OrganizationID: ws.OrganizationID,
		TemplateIDs:    []uuid.UUID{},
		MaxWorkspaces:  5,
		AllowDelete:    true,
	})
	require.Empty(t, grant.TemplateIDs)
	require.Equal(t, int32(5), grant.MaxWorkspaces)
	require.True(t, grant.AllowDelete)
	require.Empty(t, grant.NamePrefix)

	// The owner and organization of a grant never change.
	_, err := db.UpsertWorkspaceAdmissionGrant(ctx, database.UpsertWorkspaceAdmissionGrantParams{
		ID:             grant.ID,
		OwnerID:        uuid.New(),
		OrganizationID: ws.OrganizationID,
		TemplateIDs:    []uuid.UUID{},
		ExpiresAt:      dbtime.Now().Add(time.Hour),
		Now:            dbtime.Now(),
	})
	require.ErrorIs(t, err, sql.ErrNoRows)

	// Revocation is final and keeps its first time.
	revokedAt := dbtime.Now()
	revoked, err := db.RevokeWorkspaceAdmissionGrant(ctx, database.RevokeWorkspaceAdmissionGrantParams{ID: grant.ID, Now: revokedAt})
	require.NoError(t, err)
	require.True(t, revoked.RevokedAt.Valid)
	again, err := db.RevokeWorkspaceAdmissionGrant(ctx, database.RevokeWorkspaceAdmissionGrantParams{ID: grant.ID, Now: revokedAt.Add(time.Minute)})
	require.NoError(t, err)
	require.Equal(t, revoked.RevokedAt.Time, again.RevokedAt.Time)
	require.Equal(t, revoked.UpdatedAt, again.UpdatedAt)
	remirrored := upsertGrant(t, db, database.UpsertWorkspaceAdmissionGrantParams{
		ID:             grant.ID,
		OwnerID:        ws.OwnerID,
		OrganizationID: ws.OrganizationID,
		TemplateIDs:    []uuid.UUID{},
		MaxWorkspaces:  5,
	})
	require.True(t, remirrored.RevokedAt.Valid)
	require.Equal(t, revoked.RevokedAt.Time, remirrored.RevokedAt.Time)

	_, err = db.RevokeWorkspaceAdmissionGrant(ctx, database.RevokeWorkspaceAdmissionGrantParams{ID: "g-missing", Now: revokedAt})
	require.ErrorIs(t, err, sql.ErrNoRows)

	err = db.InTx(func(tx database.Store) error {
		locked, err := tx.GetWorkspaceAdmissionGrantByIDWithLock(ctx, grant.ID)
		if err != nil {
			return err
		}
		require.Equal(t, grant.ID, locked.ID)
		_, err = tx.GetWorkspaceAdmissionGrantByIDWithLock(ctx, "g-missing")
		require.ErrorIs(t, err, sql.ErrNoRows)
		return nil
	}, nil)
	require.NoError(t, err)

	// Grant IDs are opaque controller IDs with a bounded format.
	for _, id := range []string{"", "G-UPPER", "-leading-dash", "g-" + strings.Repeat("a", 70), "g/slash"} {
		_, err := db.UpsertWorkspaceAdmissionGrant(ctx, database.UpsertWorkspaceAdmissionGrantParams{
			ID:             id,
			OwnerID:        ws.OwnerID,
			OrganizationID: ws.OrganizationID,
			TemplateIDs:    []uuid.UUID{},
			ExpiresAt:      dbtime.Now().Add(time.Hour),
			Now:            dbtime.Now(),
		})
		require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionGrantsIDCheck), "id %q: %v", id, err)
	}
	upsertGrant(t, db, database.UpsertWorkspaceAdmissionGrantParams{
		ID:             uuid.NewString(),
		OwnerID:        ws.OwnerID,
		OrganizationID: ws.OrganizationID,
		TemplateIDs:    []uuid.UUID{},
	})
}

func TestWorkspaceAdmissionCreateOperations(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	resp := managedWorkspace(t, db)
	ws := resp.Workspace
	ctx := testutil.Context(t, testutil.WaitLong)
	grantID := "g-01jbx5870000000000000000bb"

	count, err := db.CountLiveWorkspaceAdmissionCreateOperationsByGrantID(ctx, grantID)
	require.NoError(t, err)
	require.Zero(t, count)

	source := json.RawMessage(`{"kind":"agent_grant","grant_id":"` + grantID + `"}`)
	op, err := db.InsertWorkspaceAdmissionCreateOperation(ctx, database.InsertWorkspaceAdmissionCreateOperationParams{
		OperationID:      uuid.New(),
		WorkspaceID:      ws.ID,
		WorkspaceBuildID: resp.Build.ID,
		JobID:            resp.Build.JobID,
		OrganizationID:   ws.OrganizationID,
		OwnerID:          ws.OwnerID,
		GrantID:          sql.NullString{String: grantID, Valid: true},
		RunID:            sql.NullString{String: "r-01jbx5870000000000000000rr", Valid: true},
		Source:           source,
		CreatedAt:        dbtime.Now(),
	})
	require.NoError(t, err)

	got, err := db.GetWorkspaceAdmissionCreateOperationByOperationID(ctx, op.OperationID)
	require.NoError(t, err)
	require.Equal(t, op.WorkspaceID, got.WorkspaceID)
	require.JSONEq(t, string(source), string(got.Source))
	got, err = db.GetWorkspaceAdmissionCreateOperationByWorkspaceID(ctx, ws.ID)
	require.NoError(t, err)
	require.Equal(t, op.OperationID, got.OperationID)

	count, err = db.CountLiveWorkspaceAdmissionCreateOperationsByGrantID(ctx, grantID)
	require.NoError(t, err)
	require.Equal(t, int32(1), count)

	// A deleted workspace no longer counts against the grant.
	require.NoError(t, db.UpdateWorkspaceDeletedByID(ctx, database.UpdateWorkspaceDeletedByIDParams{ID: ws.ID, Deleted: true}))
	count, err = db.CountLiveWorkspaceAdmissionCreateOperationsByGrantID(ctx, grantID)
	require.NoError(t, err)
	require.Zero(t, count)

	// An agent grant create must name its grant.
	other := managedWorkspace(t, db)
	_, err = db.InsertWorkspaceAdmissionCreateOperation(ctx, database.InsertWorkspaceAdmissionCreateOperationParams{
		OperationID:      uuid.New(),
		WorkspaceID:      other.Workspace.ID,
		WorkspaceBuildID: other.Build.ID,
		JobID:            other.Build.JobID,
		OrganizationID:   other.Workspace.OrganizationID,
		OwnerID:          other.Workspace.OwnerID,
		Source:           json.RawMessage(`{"kind":"agent_grant"}`),
		CreatedAt:        dbtime.Now(),
	})
	require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionCreateOperationsGrantCheck), "%v", err)
	_, err = db.InsertWorkspaceAdmissionCreateOperation(ctx, database.InsertWorkspaceAdmissionCreateOperationParams{
		OperationID:      uuid.New(),
		WorkspaceID:      other.Workspace.ID,
		WorkspaceBuildID: other.Build.ID,
		JobID:            other.Build.JobID,
		OrganizationID:   other.Workspace.OrganizationID,
		OwnerID:          other.Workspace.OwnerID,
		RunID:            sql.NullString{String: "R-UPPER", Valid: true},
		Source:           json.RawMessage(`{"kind":"employee"}`),
		CreatedAt:        dbtime.Now(),
	})
	require.True(t, database.IsCheckViolation(err, database.CheckWorkspaceAdmissionCreateOperationsRunIDCheck), "%v", err)

	// Delete operations keep the grant they ran under.
	del, err := db.InsertWorkspaceAdmissionDeleteOperation(ctx, database.InsertWorkspaceAdmissionDeleteOperationParams{
		OperationID:      uuid.New(),
		WorkspaceID:      ws.ID,
		WorkspaceBuildID: resp.Build.ID,
		JobID:            resp.Build.JobID,
		OrganizationID:   ws.OrganizationID,
		GrantID:          sql.NullString{String: grantID, Valid: true},
		CreatedAt:        dbtime.Now(),
	})
	require.NoError(t, err)
	gotDel, err := db.GetWorkspaceAdmissionDeleteOperationByOperationID(ctx, del.OperationID)
	require.NoError(t, err)
	require.Equal(t, grantID, gotDel.GrantID.String)
	require.False(t, gotDel.CanceledJobID.Valid)
}
