package admissionapi_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbfake"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissionapi"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

// agentWorkspace creates a running workspace of the member on the managed
// template, with one agent that never connected.
func (e apiEnv) agentWorkspace(t *testing.T) dbfake.WorkspaceResponse {
	t.Helper()
	return dbfake.WorkspaceBuild(t, e.db, database.WorkspaceTable{
		OrganizationID: e.orgID,
		OwnerID:        e.memberID,
		TemplateID:     e.template.ID,
	}).WithAgent().Do()
}

func (e apiEnv) recordWakeIntent(t *testing.T, ws database.WorkspaceTable, agentID uuid.UUID) database.WorkspaceAdmissionWakeIntent {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitShort)
	now := dbtime.Now()
	intent, err := e.db.UpsertWorkspaceAdmissionWakeIntent(ctx, database.UpsertWorkspaceAdmissionWakeIntentParams{
		WorkspaceID:      ws.ID,
		OrganizationID:   ws.OrganizationID,
		AgentID:          agentID,
		Source:           "pty",
		RequestedBy:      uuid.NullUUID{UUID: ws.OwnerID, Valid: true},
		FirstRequestedAt: now,
		LastRequestedAt:  now,
		RequestCount:     1,
	})
	require.NoError(t, err)
	return intent
}

// rawIntentKeys returns the JSON keys of every intent in a list response.
func rawIntentKeys(t *testing.T, raw json.RawMessage) [][]string {
	t.Helper()
	var body struct {
		Intents []map[string]json.RawMessage `json:"intents"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	require.NotNil(t, body.Intents, "intents must be a list, got %s", raw)
	out := make([][]string, 0, len(body.Intents))
	for _, intent := range body.Intents {
		keys := make([]string, 0, len(intent))
		for k := range intent {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out = append(out, keys)
	}
	return out
}

func TestWakeIntents(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)

	var raw json.RawMessage
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/wake-intents?pending=true", nil, &raw))
	require.Empty(t, rawIntentKeys(t, raw))

	ws := env.agentWorkspace(t)
	recorded := env.recordWakeIntent(t, ws.Workspace, ws.Agents[0].ID)

	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/wake-intents?pending=true&limit=100", nil, &raw))
	// The keys are the controller contract; acknowledgement fields are
	// omitted while they are null.
	require.Equal(t, [][]string{{
		"agent_id", "first_requested_at", "last_requested_at", "request_count", "requested_by", "source", "workspace_id",
	}}, rawIntentKeys(t, raw))
	var list admissionapi.ListWakeIntentsResponse
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Intents, 1)
	intent := list.Intents[0]
	require.Equal(t, ws.Workspace.ID, intent.WorkspaceID)
	require.Equal(t, ws.Agents[0].ID, intent.AgentID)
	require.Equal(t, "pty", intent.Source)
	require.Equal(t, env.memberID.String(), intent.RequestedBy)
	require.Equal(t, 1, intent.RequestCount)
	require.True(t, intent.LastRequestedAt.Equal(recorded.LastRequestedAt))

	// Validation.
	var errResp admissionapi.ErrorResponse
	for _, path := range []string{"/v1/wake-intents?pending=maybe", "/v1/wake-intents?limit=0", "/v1/wake-intents?limit=1001"} {
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodGet, path, nil, &errResp), path)
		require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code)
	}
	ackPath := "/v1/wake-intents/" + ws.Workspace.ID.String() + "/ack"
	for _, body := range []any{
		admissionapi.AckWakeIntentRequest{PowerEpoch: 1, Result: admissionapi.WakeResultWoken},
		admissionapi.AckWakeIntentRequest{LastRequestedAt: intent.LastRequestedAt, PowerEpoch: 1, Result: "asleep"},
		admissionapi.AckWakeIntentRequest{LastRequestedAt: intent.LastRequestedAt, PowerEpoch: -1, Result: admissionapi.WakeResultWoken},
		map[string]any{"last_requested_at": intent.LastRequestedAt, "power_epoch": 1, "result": "woken", "extra": true},
	} {
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, ackPath, body, &errResp), "%+v", body)
		require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code)
	}

	// An acknowledgement of a request the controller did not see is
	// refused, as is one for a workspace without an intent.
	var ack admissionapi.AckIntentResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, ackPath, admissionapi.AckWakeIntentRequest{
		LastRequestedAt: intent.LastRequestedAt.Add(-time.Second),
		PowerEpoch:      5,
		Result:          admissionapi.WakeResultWoken,
	}, &ack))
	require.False(t, ack.Acked)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/wake-intents/"+uuid.NewString()+"/ack", admissionapi.AckWakeIntentRequest{
		LastRequestedAt: intent.LastRequestedAt,
		PowerEpoch:      5,
		Result:          admissionapi.WakeResultWoken,
	}, &ack))
	require.False(t, ack.Acked)

	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, ackPath, admissionapi.AckWakeIntentRequest{
		LastRequestedAt: intent.LastRequestedAt,
		PowerEpoch:      5,
		Result:          admissionapi.WakeResultWoken,
	}, &ack))
	require.True(t, ack.Acked)

	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/wake-intents?pending=true", nil, &raw))
	require.Empty(t, rawIntentKeys(t, raw))
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/wake-intents", nil, &raw))
	require.Equal(t, [][]string{{
		"acked_at", "acked_power_epoch", "agent_id", "first_requested_at", "last_requested_at", "request_count", "requested_by", "source", "workspace_id",
	}}, rawIntentKeys(t, raw))
	require.NoError(t, json.Unmarshal(raw, &list))
	require.NotNil(t, list.Intents[0].AckedAt)
	require.NotNil(t, list.Intents[0].AckedPowerEpoch)
	require.EqualValues(t, 5, *list.Intents[0].AckedPowerEpoch)

	// A new request after the acknowledgement is pending again.
	env.recordWakeIntent(t, ws.Workspace, ws.Agents[0].ID)
	var pending admissionapi.ListWakeIntentsResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/wake-intents?pending=true", nil, &pending))
	require.Len(t, pending.Intents, 1)
	require.Nil(t, pending.Intents[0].AckedAt)
	require.Nil(t, pending.Intents[0].AckedPowerEpoch)

	for _, result := range []string{admissionapi.WakeResultAlreadyRunning, admissionapi.WakeResultFailed, admissionapi.WakeResultIgnored} {
		require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, ackPath, admissionapi.AckWakeIntentRequest{
			LastRequestedAt: pending.Intents[0].LastRequestedAt,
			PowerEpoch:      6,
			Result:          result,
		}, &ack), result)
		require.True(t, ack.Acked, result)
	}
}

// TestWatchWakesOnWakeIntents checks that the long poll returns for a
// recorded wake intent even when no admission record changed.
func TestWatchWakesOnWakeIntents(t *testing.T) {
	t.Parallel()
	// The watch timeout never fires on the mock clock, so only a
	// notification ends the long poll.
	clock := quartz.NewMock(t)
	env := newAPIEnv(t, clock)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	ws := env.agentWorkspace(t)

	var initial admissionapi.WatchResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/watch", nil, &initial))

	trap := clock.Trap().NewTimer("admissionapi", "watch")
	defer trap.Close()
	done := make(chan admissionapi.WatchResponse, 1)
	go func() {
		var resp admissionapi.WatchResponse
		_ = env.call(ctx, t, http.MethodGet, "/v1/watch?timeout_seconds=30&cursor="+initial.Cursor, nil, &resp)
		done <- resp
	}()
	trap.MustWait(ctx).MustRelease(ctx)

	// Record the intent the way the wake intent recorder does.
	env.recordWakeIntent(t, ws.Workspace, ws.Agents[0].ID)
	require.NoError(t, workspaceadmission.PublishIntent(env.ps, workspaceadmission.EventKindWakeIntent, ws.Workspace.ID))
	resp := testutil.TryReceive(ctx, t, done)
	require.True(t, resp.Changed)
	require.NotEqual(t, initial.Cursor, resp.Cursor)

	// Without a notification the cursor still moves.
	env.recordWakeIntent(t, ws.Workspace, ws.Agents[0].ID)
	var after admissionapi.WatchResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/watch?cursor="+resp.Cursor, nil, &after))
	require.True(t, after.Changed)
	require.NotEqual(t, resp.Cursor, after.Cursor)
}

func TestKeepaliveIntents(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ws := env.agentWorkspace(t)
	ctx := testutil.Context(t, testutil.WaitSuperLong)

	var raw json.RawMessage
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/keepalive-intents?pending=true", nil, &raw))
	require.Empty(t, rawIntentKeys(t, raw))

	// The workspace owner presses "Keep awake 2 h" on the workspace page.
	resp, err := env.member.WorkspaceCooplayKeepalive(ctx, ws.Workspace.ID, codersdk.WorkspaceCooplayKeepaliveRequest{})
	require.NoError(t, err)
	require.Equal(t, codersdk.WorkspaceCooplayKeepaliveDefaultSeconds, resp.ForSeconds)

	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/keepalive-intents?pending=true&limit=100", nil, &raw))
	// The keys are the controller contract; acked_at is omitted while it is
	// null.
	require.Equal(t, [][]string{{"for_seconds", "requested_at", "requested_by", "workspace_id"}}, rawIntentKeys(t, raw))
	var list admissionapi.ListKeepaliveIntentsResponse
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Intents, 1)
	intent := list.Intents[0]
	require.Equal(t, ws.Workspace.ID, intent.WorkspaceID)
	require.Equal(t, env.memberID, intent.RequestedBy)
	require.Equal(t, 7200, intent.ForSeconds)
	require.True(t, intent.RequestedAt.Equal(resp.RequestedAt))

	// Validation.
	var errResp admissionapi.ErrorResponse
	for _, path := range []string{"/v1/keepalive-intents?pending=maybe", "/v1/keepalive-intents?limit=0", "/v1/keepalive-intents?limit=1001"} {
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodGet, path, nil, &errResp), path)
		require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code)
	}
	ackPath := "/v1/keepalive-intents/" + ws.Workspace.ID.String() + "/ack"
	until := dbtime.Now().Add(2 * time.Hour)
	for _, body := range []any{
		// requested_at is required.
		admissionapi.AckKeepaliveIntentRequest{Until: &until, Result: admissionapi.KeepaliveResultApplied},
		// Unknown result.
		admissionapi.AckKeepaliveIntentRequest{RequestedAt: intent.RequestedAt, Until: &until, Result: "extended"},
		// An applied keepalive names its end.
		admissionapi.AckKeepaliveIntentRequest{RequestedAt: intent.RequestedAt, Result: admissionapi.KeepaliveResultApplied},
		map[string]any{"requested_at": intent.RequestedAt, "until": time.Time{}, "result": "applied"},
		map[string]any{"requested_at": intent.RequestedAt, "until": until, "result": "applied", "extra": true},
	} {
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, ackPath, body, &errResp), "%+v", body)
		require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code)
	}

	// An acknowledgement of a request the controller did not see is
	// refused, as is one for a workspace without an intent.
	var ack admissionapi.AckIntentResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, ackPath, admissionapi.AckKeepaliveIntentRequest{
		RequestedAt: intent.RequestedAt.Add(-time.Second),
		Until:       &until,
		Result:      admissionapi.KeepaliveResultApplied,
	}, &ack))
	require.False(t, ack.Acked)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/keepalive-intents/"+uuid.NewString()+"/ack", admissionapi.AckKeepaliveIntentRequest{
		RequestedAt: intent.RequestedAt,
		Until:       &until,
		Result:      admissionapi.KeepaliveResultApplied,
	}, &ack))
	require.False(t, ack.Acked)

	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, ackPath, admissionapi.AckKeepaliveIntentRequest{
		RequestedAt: intent.RequestedAt,
		Until:       &until,
		Result:      admissionapi.KeepaliveResultApplied,
	}, &ack))
	require.True(t, ack.Acked)

	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/keepalive-intents?pending=true", nil, &raw))
	require.Empty(t, rawIntentKeys(t, raw))
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/keepalive-intents", nil, &raw))
	require.Equal(t, [][]string{{"acked_at", "for_seconds", "requested_at", "requested_by", "workspace_id"}}, rawIntentKeys(t, raw))
	rows, err := env.db.GetWorkspaceAdmissionKeepaliveIntents(ctx, database.GetWorkspaceAdmissionKeepaliveIntentsParams{LimitCount: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, sql.NullString{String: admissionapi.KeepaliveResultApplied, Valid: true}, rows[0].AckedResult)
	require.True(t, rows[0].AckedUntil.Time.Equal(dbtime.Time(until)))

	// A new request after the acknowledgement is pending again. A denial
	// needs no end; the controller client sends a zero time.
	resp, err = env.member.WorkspaceCooplayKeepalive(ctx, ws.Workspace.ID, codersdk.WorkspaceCooplayKeepaliveRequest{ForSeconds: 600})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/keepalive-intents?pending=true", nil, &list))
	require.Len(t, list.Intents, 1)
	require.Equal(t, 600, list.Intents[0].ForSeconds)
	require.Nil(t, list.Intents[0].AckedAt)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, ackPath, map[string]any{
		"requested_at": resp.RequestedAt,
		"until":        time.Time{},
		"result":       admissionapi.KeepaliveResultDenied,
	}, &ack))
	require.True(t, ack.Acked)
	rows, err = env.db.GetWorkspaceAdmissionKeepaliveIntents(ctx, database.GetWorkspaceAdmissionKeepaliveIntentsParams{LimitCount: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, sql.NullString{String: admissionapi.KeepaliveResultDenied, Valid: true}, rows[0].AckedResult)
	require.False(t, rows[0].AckedUntil.Valid)
}

// TestWatchWakesOnKeepaliveIntents checks that the long poll returns for a
// keepalive request on the workspace page even when no admission record
// changed.
func TestWatchWakesOnKeepaliveIntents(t *testing.T) {
	t.Parallel()
	// The watch timeout never fires on the mock clock, so only a
	// notification ends the long poll.
	clock := quartz.NewMock(t)
	env := newAPIEnv(t, clock)
	ws := env.agentWorkspace(t)
	ctx := testutil.Context(t, testutil.WaitSuperLong)

	var initial admissionapi.WatchResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/watch", nil, &initial))

	trap := clock.Trap().NewTimer("admissionapi", "watch")
	defer trap.Close()
	done := make(chan admissionapi.WatchResponse, 1)
	go func() {
		var resp admissionapi.WatchResponse
		_ = env.call(ctx, t, http.MethodGet, "/v1/watch?timeout_seconds=30&cursor="+initial.Cursor, nil, &resp)
		done <- resp
	}()
	trap.MustWait(ctx).MustRelease(ctx)

	_, err := env.member.WorkspaceCooplayKeepalive(ctx, ws.Workspace.ID, codersdk.WorkspaceCooplayKeepaliveRequest{})
	require.NoError(t, err)
	resp := testutil.TryReceive(ctx, t, done)
	require.True(t, resp.Changed)
	require.NotEqual(t, initial.Cursor, resp.Cursor)
}
