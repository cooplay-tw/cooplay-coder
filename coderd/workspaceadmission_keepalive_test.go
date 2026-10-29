package coderd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
)

// keepaliveIntent returns the pending keepalive intent of a workspace.
func keepaliveIntent(t *testing.T, db database.Store, workspaceID uuid.UUID) (database.WorkspaceAdmissionKeepaliveIntent, bool) {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitShort)
	rows, err := db.GetWorkspaceAdmissionKeepaliveIntents(ctx, database.GetWorkspaceAdmissionKeepaliveIntentsParams{
		PendingOnly: true,
		LimitCount:  1000,
	})
	require.NoError(t, err)
	for _, row := range rows {
		if row.WorkspaceID == workspaceID {
			return row, true
		}
	}
	return database.WorkspaceAdmissionKeepaliveIntent{}, false
}

func requireSDKStatus(t *testing.T, err error, status int) {
	t.Helper()
	var sdkErr *codersdk.Error
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, status, sdkErr.StatusCode())
}

func TestWorkspaceCooplayKeepalive(t *testing.T) {
	t.Parallel()

	t.Run("RecordsIntent", func(t *testing.T) {
		t.Parallel()
		env := newWakeEnv(t)
		ws := env.managedWorkspace(t)
		ctx := testutil.Context(t, testutil.WaitLong)

		events := make(chan workspaceadmission.Event, 10)
		cancel, err := env.api.Pubsub.Subscribe(workspaceadmission.EventChannel, func(_ context.Context, msg []byte) {
			var ev workspaceadmission.Event
			if json.Unmarshal(msg, &ev) == nil {
				events <- ev
			}
		})
		require.NoError(t, err)
		defer cancel()

		// Without a duration the request asks for two hours.
		resp, err := env.member.WorkspaceCooplayKeepalive(ctx, ws.Workspace.ID, codersdk.WorkspaceCooplayKeepaliveRequest{})
		require.NoError(t, err)
		require.Equal(t, codersdk.WorkspaceCooplayKeepaliveDefaultSeconds, resp.ForSeconds)
		intent, ok := keepaliveIntent(t, env.db, ws.Workspace.ID)
		require.True(t, ok)
		require.Equal(t, env.memberID, intent.RequestedBy)
		require.EqualValues(t, 7200, intent.ForSeconds)
		require.True(t, intent.RequestedAt.Equal(resp.RequestedAt))

		ev := testutil.TryReceive(ctx, t, events)
		require.Equal(t, workspaceadmission.EventKindKeepaliveIntent, ev.Kind)
		require.NotNil(t, ev.WorkspaceID)
		require.Equal(t, ws.Workspace.ID, *ev.WorkspaceID)

		// A later request replaces the pending one.
		resp, err = env.member.WorkspaceCooplayKeepalive(ctx, ws.Workspace.ID, codersdk.WorkspaceCooplayKeepaliveRequest{ForSeconds: 600})
		require.NoError(t, err)
		require.Equal(t, 600, resp.ForSeconds)
		replaced, ok := keepaliveIntent(t, env.db, ws.Workspace.ID)
		require.True(t, ok)
		require.EqualValues(t, 600, replaced.ForSeconds)
		require.True(t, replaced.RequestedAt.After(intent.RequestedAt))

		// A request without a body, as the workspace page sends it, uses the
		// default.
		res, err := env.member.Request(ctx, http.MethodPost, "/api/v2/workspaces/"+ws.Workspace.ID.String()+"/cooplay/keepalive", nil)
		require.NoError(t, err)
		defer res.Body.Close()
		require.Equal(t, http.StatusAccepted, res.StatusCode)
		noBody, ok := keepaliveIntent(t, env.db, ws.Workspace.ID)
		require.True(t, ok)
		require.EqualValues(t, codersdk.WorkspaceCooplayKeepaliveDefaultSeconds, noBody.ForSeconds)

		// Administrators who may update the workspace can request it too.
		_, err = env.owner.WorkspaceCooplayKeepalive(ctx, ws.Workspace.ID, codersdk.WorkspaceCooplayKeepaliveRequest{})
		require.NoError(t, err)
		byOwner, ok := keepaliveIntent(t, env.db, ws.Workspace.ID)
		require.True(t, ok)
		require.NotEqual(t, env.memberID, byOwner.RequestedBy)
	})

	t.Run("InvalidDuration", func(t *testing.T) {
		t.Parallel()
		env := newWakeEnv(t)
		ws := env.managedWorkspace(t)
		ctx := testutil.Context(t, testutil.WaitLong)

		for _, seconds := range []int{-1, codersdk.WorkspaceCooplayKeepaliveMaxSeconds + 1} {
			_, err := env.member.WorkspaceCooplayKeepalive(ctx, ws.Workspace.ID, codersdk.WorkspaceCooplayKeepaliveRequest{ForSeconds: seconds})
			requireSDKStatus(t, err, http.StatusBadRequest)
		}
		_, ok := keepaliveIntent(t, env.db, ws.Workspace.ID)
		require.False(t, ok)
		_, err := env.member.WorkspaceCooplayKeepalive(ctx, ws.Workspace.ID, codersdk.WorkspaceCooplayKeepaliveRequest{ForSeconds: codersdk.WorkspaceCooplayKeepaliveMaxSeconds})
		require.NoError(t, err)
	})

	t.Run("UnmanagedTemplate", func(t *testing.T) {
		t.Parallel()
		env := newWakeEnv(t)
		ws := env.workspace(t)
		ctx := testutil.Context(t, testutil.WaitLong)

		_, err := env.member.WorkspaceCooplayKeepalive(ctx, ws.Workspace.ID, codersdk.WorkspaceCooplayKeepaliveRequest{})
		requireSDKStatus(t, err, http.StatusBadRequest)
		_, ok := keepaliveIntent(t, env.db, ws.Workspace.ID)
		require.False(t, ok)
	})

	t.Run("OtherUser", func(t *testing.T) {
		t.Parallel()
		env := newWakeEnv(t)
		ws := env.managedWorkspace(t)
		other, _ := coderdtest.CreateAnotherUser(t, env.owner, env.orgID)
		ctx := testutil.Context(t, testutil.WaitLong)

		// A user who cannot read the workspace does not learn that it
		// exists.
		_, err := other.WorkspaceCooplayKeepalive(ctx, ws.Workspace.ID, codersdk.WorkspaceCooplayKeepaliveRequest{})
		requireSDKStatus(t, err, http.StatusNotFound)
		_, ok := keepaliveIntent(t, env.db, ws.Workspace.ID)
		require.False(t, ok)
	})
}
