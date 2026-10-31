package admissionapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbfake"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/util/ptr"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissionapi"
	"github.com/coder/coder/v2/coderd/wspubsub"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

// fullState is a state push with every field set, as the controller sends
// it.
func fullState(power codersdk.WorkspaceCooplayPowerState, epoch int64) admissionapi.WorkspaceCooplayState {
	now := dbtime.Now()
	return admissionapi.WorkspaceCooplayState{
		PowerState:     power,
		PowerEpoch:     epoch,
		Generation:     2,
		Since:          now.Add(-time.Minute),
		Reason:         "idle",
		SleepNoticeAt:  ptr.Ref(now.Add(2 * time.Minute)),
		KeepaliveUntil: ptr.Ref(now.Add(time.Hour)),
		Runs: []codersdk.WorkspaceCooplayRun{{
			ID:            "r-01jrun",
			Harness:       "claude-code",
			Mode:          "A",
			State:         codersdk.WorkspaceCooplayRunWaitingHuman,
			WaitingReason: "permission",
			StartedBy:     codersdk.WorkspaceCooplayRunStartedByGrant,
			GrantID:       "g-01jgrant",
			LastEventAt:   now,
		}},
		LastJob: &codersdk.WorkspaceCooplayJob{
			ID:            "j-1",
			Name:          "tests",
			Kind:          "exec",
			ArgvSummary:   "go test ./...",
			ArtifactsPath: "/home/dev/work/artifacts/j-1",
			RunID:         "r-01jrun",
			StartedAt:     now.Add(-time.Minute),
			EndedAt:       ptr.Ref(now),
			ExitCode:      ptr.Ref(1),
		},
		Git: &codersdk.WorkspaceCooplayGit{
			Branch:           "feat/sleep",
			Base:             "main",
			Dirty:            true,
			UncommittedFiles: 3,
			Ahead:            2,
			Behind:           1,
			MergeConflict:    ptr.Ref(false),
			ReportedAt:       now,
		},
		PR: &codersdk.WorkspaceCooplayPR{
			URL:        "https://github.com/example/repo/pull/7",
			State:      "open",
			CI:         "failing",
			Number:     7,
			ReportedAt: now,
		},
		LastSample: json.RawMessage(`{"memory_used_bytes":1024,"at":"2026-10-30T00:00:00Z"}`),
	}
}

// objectKeys returns the sorted keys of a JSON object.
func objectKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &obj))
	return sortedKeys(obj)
}

// TestWorkspaceState checks PUT and DELETE /v1/workspaces/{id}/state: the
// exact keys, the compare-and-set on the power epoch, also after the
// controller reported the workspace as off, every power state, and what
// the workspace owner then sees on the workspace.
func TestWorkspaceState(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ws := env.agentWorkspace(t)
	// All power states, epoch checks and clear/replay calls share this context.
	ctx := testutil.Context(t, 2*testutil.WaitSuperLong)
	path := "/v1/workspaces/" + ws.Workspace.ID.String() + "/state"

	events := make(chan wspubsub.WorkspaceEvent, 16)
	cancel, err := env.ps.SubscribeWithErr(wspubsub.WorkspaceEventChannel(env.memberID), wspubsub.HandleWorkspaceEvent(
		func(_ context.Context, e wspubsub.WorkspaceEvent, err error) {
			if err == nil && e.WorkspaceID == ws.Workspace.ID {
				events <- e
			}
		}))
	require.NoError(t, err)
	defer cancel()

	// A full state is stored and answered with exactly the contract keys.
	state := fullState(codersdk.WorkspaceCooplayPowerHibernated, 5)
	var raw json.RawMessage
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, path, state, &raw))
	require.Equal(t, []string{
		"generation", "git", "keepalive_until", "last_job", "last_sample", "power_epoch", "power_state",
		"pr", "reason", "runs", "since", "sleep_notice_at",
	}, objectKeys(t, raw))
	var body struct {
		Runs    []json.RawMessage `json:"runs"`
		LastJob json.RawMessage   `json:"last_job"`
		Git     json.RawMessage   `json:"git"`
		PR      json.RawMessage   `json:"pr"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Len(t, body.Runs, 1)
	require.Equal(t, []string{"grant_id", "harness", "id", "last_event_at", "mode", "started_by", "state", "waiting_reason"}, objectKeys(t, body.Runs[0]))
	require.Equal(t, []string{"argv_summary", "artifacts_path", "ended_at", "exit_code", "id", "kind", "name", "run_id", "started_at"}, objectKeys(t, body.LastJob))
	require.Equal(t, []string{"ahead", "base", "behind", "branch", "dirty", "merge_conflict", "reported_at", "uncommitted_files"}, objectKeys(t, body.Git))
	require.Equal(t, []string{"ci", "number", "reported_at", "state", "url"}, objectKeys(t, body.PR))
	var stored admissionapi.WorkspaceCooplayState
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Equal(t, state.PowerState, stored.PowerState)
	require.EqualValues(t, 5, stored.PowerEpoch)
	require.EqualValues(t, 2, stored.Generation)
	require.True(t, stored.Since.Equal(state.Since))
	require.True(t, stored.SleepNoticeAt.Equal(*state.SleepNoticeAt))
	require.Equal(t, state.Runs[0].ID, stored.Runs[0].ID)
	require.Equal(t, *state.LastJob.ExitCode, *stored.LastJob.ExitCode)
	require.Equal(t, state.Git.Branch, stored.Git.Branch)
	require.Equal(t, state.PR.URL, stored.PR.URL)
	require.JSONEq(t, string(state.LastSample), string(stored.LastSample))
	require.Equal(t, wspubsub.WorkspaceEventKindStateChange, testutil.TryReceive(ctx, t, events).Kind)

	// The owner sees the power state, Runs and facts, never the controller
	// internals, and a sleeping workspace is not unhealthy.
	workspace, err := env.member.Workspace(ctx, ws.Workspace.ID)
	require.NoError(t, err)
	require.NotNil(t, workspace.Cooplay)
	require.NotNil(t, workspace.Cooplay.Power)
	require.Equal(t, codersdk.WorkspaceCooplayPowerHibernated, workspace.Cooplay.Power.State)
	require.Equal(t, "idle", workspace.Cooplay.Power.Reason)
	require.True(t, workspace.Cooplay.Power.Since.Equal(state.Since))
	require.True(t, workspace.Cooplay.SleepNoticeAt.Equal(*state.SleepNoticeAt))
	require.True(t, workspace.Cooplay.KeepaliveUntil.Equal(*state.KeepaliveUntil))
	require.Equal(t, state.Runs[0].ID, workspace.Cooplay.Runs[0].ID)
	require.Equal(t, codersdk.WorkspaceCooplayRunWaitingHuman, workspace.Cooplay.Runs[0].State)
	require.Equal(t, state.LastJob.ArgvSummary, workspace.Cooplay.LastJob.ArgvSummary)
	require.Equal(t, 3, workspace.Cooplay.Git.UncommittedFiles)
	require.Equal(t, 7, workspace.Cooplay.PR.Number)
	require.Nil(t, workspace.Cooplay.Source)
	userRaw, err := json.Marshal(workspace.Cooplay)
	require.NoError(t, err)
	require.Equal(t, []string{"git", "keepalive_until", "last_job", "power", "pr", "runs", "sleep_notice_at"}, objectKeys(t, userRaw))
	require.True(t, workspace.Health.Healthy)

	// An older epoch is rejected and changes nothing.
	var errResp admissionapi.ErrorResponse
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPut, path, fullState(codersdk.WorkspaceCooplayPowerRunning, 4), &errResp))
	require.Equal(t, admissionapi.CodeStalePowerEpoch, errResp.Code)
	workspace, err = env.member.Workspace(ctx, ws.Workspace.ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.WorkspaceCooplayPowerHibernated, workspace.Cooplay.Power.State)

	// An equal epoch replaces the whole state; omitted facts are cleared.
	minimal := admissionapi.WorkspaceCooplayState{
		PowerState: codersdk.WorkspaceCooplayPowerWaking,
		PowerEpoch: 5,
		Generation: 2,
		Since:      dbtime.Now(),
	}
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, path, minimal, &raw))
	require.Equal(t, []string{"generation", "power_epoch", "power_state", "reason", "since"}, objectKeys(t, raw))
	workspace, err = env.member.Workspace(ctx, ws.Workspace.ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.WorkspaceCooplayPowerWaking, workspace.Cooplay.Power.State)
	require.Empty(t, workspace.Cooplay.Runs)
	require.NotNil(t, workspace.Cooplay.Runs, "runs is always a list")
	require.Nil(t, workspace.Cooplay.LastJob)
	require.Nil(t, workspace.Cooplay.Git)
	require.Nil(t, workspace.Cooplay.SleepNoticeAt)

	// Every power state is accepted, each at a newer epoch.
	for i, power := range codersdk.WorkspaceCooplayPowerStates {
		st := minimal
		st.PowerState = power
		st.PowerEpoch = int64(6 + i)
		require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, path, st, nil), power)
		workspace, err = env.member.Workspace(ctx, ws.Workspace.ID)
		require.NoError(t, err)
		require.Equal(t, power, workspace.Cooplay.Power.State)
	}
	lastEpoch := int64(5 + len(codersdk.WorkspaceCooplayPowerStates))

	// Off: no power state is shown, but the epoch is kept, so a delayed
	// older push is still rejected while the same epoch is accepted.
	for range 2 {
		require.Equal(t, http.StatusNoContent, env.call(ctx, t, http.MethodDelete, path, nil, nil))
	}
	workspace, err = env.member.Workspace(ctx, ws.Workspace.ID)
	require.NoError(t, err)
	require.Nil(t, workspace.Cooplay)
	stale := minimal
	stale.PowerEpoch = lastEpoch - 1
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPut, path, stale, &errResp))
	require.Equal(t, admissionapi.CodeStalePowerEpoch, errResp.Code)
	again := minimal
	again.PowerEpoch = lastEpoch
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, path, again, nil))
	workspace, err = env.member.Workspace(ctx, ws.Workspace.ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.WorkspaceCooplayPowerWaking, workspace.Cooplay.Power.State)
}

// TestWorkspaceStateInvalid checks that invalid pushes fail with
// invalid_request and unknown, unmanaged and deleted workspaces are refused.
func TestWorkspaceStateInvalid(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ws := env.agentWorkspace(t)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	path := "/v1/workspaces/" + ws.Workspace.ID.String() + "/state"
	valid := fullState(codersdk.WorkspaceCooplayPowerRunning, 1)
	validRaw, err := json.Marshal(valid)
	require.NoError(t, err)
	// withField replaces one top-level field of the valid push.
	withField := func(key, value string) json.RawMessage {
		var obj map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(validRaw, &obj))
		obj[key] = json.RawMessage(value)
		out, err := json.Marshal(obj)
		require.NoError(t, err)
		return out
	}
	tooManyRuns := make([]codersdk.WorkspaceCooplayRun, 21)
	for i := range tooManyRuns {
		tooManyRuns[i] = valid.Runs[0]
	}
	tooManyRunsRaw, err := json.Marshal(tooManyRuns)
	require.NoError(t, err)

	for name, body := range map[string]json.RawMessage{
		"UnknownPowerState":  withField("power_state", `"off"`),
		"DegradedPowerState": withField("power_state", `"degraded"`),
		"TransitionState":    withField("power_state", `"transition"`),
		"NegativeEpoch":      withField("power_epoch", `-1`),
		"NegativeGeneration": withField("generation", `-1`),
		"MissingSince":       withField("since", `"0001-01-01T00:00:00Z"`),
		"LongReason":         withField("reason", `"`+strings.Repeat("r", 1025)+`"`),
		"TooManyRuns":        withField("runs", string(tooManyRunsRaw)),
		"RunStartedByAgent":  withField("runs", `[{"id":"r-1","state":"running","started_by":"agent","last_event_at":"2026-10-30T00:00:00Z"}]`),
		"RunUnknownState":    withField("runs", `[{"id":"r-1","state":"waiting","started_by":"grant","last_event_at":"2026-10-30T00:00:00Z"}]`),
		"UnknownField":       withField("power", `"running"`),
		"UnknownGitField":    withField("git", `{"branch":"main","stash":1}`),
		"LargeSample":        withField("last_sample", fmt.Sprintf(`{"pad":%q}`, strings.Repeat("x", 4096))),
		"NotJSON":            json.RawMessage(`[]`),
	} {
		var errResp admissionapi.ErrorResponse
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPut, path, body, &errResp), name)
		require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code, name)
	}
	workspace, err := env.member.Workspace(ctx, ws.Workspace.ID)
	require.NoError(t, err)
	require.Nil(t, workspace.Cooplay, "no invalid push was stored")

	var errResp admissionapi.ErrorResponse
	require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPut, "/v1/workspaces/not-a-uuid/state", valid, &errResp))
	require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		require.Equal(t, http.StatusNotFound, env.call(ctx, t, method, "/v1/workspaces/"+uuid.NewString()+"/state", valid, &errResp), method)
		require.Equal(t, admissionapi.CodeNotFound, errResp.Code)
	}

	// Workspaces of unmanaged templates are not found.
	unmanaged := dbfake.WorkspaceBuild(t, env.db, database.WorkspaceTable{
		OrganizationID: env.orgID,
		OwnerID:        env.memberID,
		TemplateID:     env.unmanaged.ID,
	}).WithAgent().Do()
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		require.Equal(t, http.StatusNotFound, env.call(ctx, t, method, "/v1/workspaces/"+unmanaged.Workspace.ID.String()+"/state", valid, &errResp), method)
		require.Equal(t, admissionapi.CodeNotFound, errResp.Code)
	}

	// A deleted workspace takes no new state, but clearing it succeeds.
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, path, valid, nil))
	require.NoError(t, env.db.UpdateWorkspaceDeletedByID(ctx, database.UpdateWorkspaceDeletedByIDParams{ID: ws.Workspace.ID, Deleted: true}))
	next := valid
	next.PowerEpoch = 2
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPut, path, next, &errResp))
	require.Equal(t, admissionapi.CodeWorkspaceDeleted, errResp.Code)
	require.Equal(t, http.StatusNoContent, env.call(ctx, t, http.MethodDelete, path, nil, nil))
}

// TestWorkspaceStateHealth checks that the agents of a managed workspace
// only count as failing while its microVM runs.
func TestWorkspaceStateHealth(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ws := env.agentWorkspace(t)
	agentID := ws.Agents[0].ID
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	path := "/v1/workspaces/" + ws.Workspace.ID.String() + "/state"

	connectedAt := dbtime.Now().Add(-time.Hour)
	require.NoError(t, env.db.UpdateWorkspaceAgentConnectionByID(ctx, database.UpdateWorkspaceAgentConnectionByIDParams{
		ID:               agentID,
		FirstConnectedAt: sql.NullTime{Time: connectedAt, Valid: true},
		LastConnectedAt:  sql.NullTime{Time: connectedAt, Valid: true},
		DisconnectedAt:   sql.NullTime{Time: connectedAt.Add(time.Second), Valid: true},
		UpdatedAt:        dbtime.Now(),
	}))

	// Without a reported power state a disconnected agent is failing.
	workspace, err := env.member.Workspace(ctx, ws.Workspace.ID)
	require.NoError(t, err)
	require.False(t, workspace.Health.Healthy)
	require.Equal(t, []uuid.UUID{agentID}, workspace.Health.FailingAgents)

	for i, power := range codersdk.WorkspaceCooplayPowerStates {
		st := admissionapi.WorkspaceCooplayState{PowerState: power, PowerEpoch: int64(i), Since: dbtime.Now()}
		require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, path, st, nil), power)
		workspace, err = env.member.Workspace(ctx, ws.Workspace.ID)
		require.NoError(t, err)
		if power.VMRunning() {
			require.False(t, workspace.Health.Healthy, power)
			require.Equal(t, []uuid.UUID{agentID}, workspace.Health.FailingAgents, power)
			continue
		}
		require.True(t, workspace.Health.Healthy, power)
		require.Empty(t, workspace.Health.FailingAgents, power)
		require.NotNil(t, workspace.Health.FailingAgents, power)
	}

	// The workspace list applies the same rule.
	workspaces, err := env.member.Workspaces(ctx, codersdk.WorkspaceFilter{})
	require.NoError(t, err)
	require.Len(t, workspaces.Workspaces, 1)
	require.Equal(t, codersdk.WorkspaceCooplayPowerUnknown, workspaces.Workspaces[0].Cooplay.Power.State)
	require.True(t, workspaces.Workspaces[0].Health.Healthy)
}

// TestWorkspaceCooplaySource checks that the owner sees which Run and grant
// created a workspace, without a reported power state.
func TestWorkspaceCooplaySource(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)

	expiresAt := dbtime.Now().Add(time.Hour)
	grant := admissionapi.GrantMirror{
		ID:             "g-01jsource",
		OwnerID:        env.memberID,
		OrganizationID: env.orgID,
		MaxWorkspaces:  1,
		ExpiresAt:      expiresAt,
	}
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, "/v1/grants/"+grant.ID, grant, nil))
	var created admissionapi.CreateWorkspaceResponse
	require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", admissionapi.CreateWorkspaceRequest{
		OperationID: uuid.New(),
		OwnerID:     env.memberID,
		TemplateID:  env.template.ID,
		Name:        "agent-made",
		Profile:     "small",
		Source: admissionapi.CreateWorkspaceSource{
			Kind:    admissionapi.CreateSourceAgentGrant,
			GrantID: grant.ID,
			RunID:   "r-01jsource",
			Harness: "codex",
		},
	}, &created))

	workspace, err := env.member.Workspace(ctx, created.WorkspaceID)
	require.NoError(t, err)
	require.NotNil(t, workspace.Cooplay)
	require.Nil(t, workspace.Cooplay.Power)
	require.Empty(t, workspace.Cooplay.Runs)
	source := workspace.Cooplay.Source
	require.NotNil(t, source)
	require.Equal(t, codersdk.WorkspaceCooplaySourceAgentGrant, source.Kind)
	require.Equal(t, grant.ID, source.GrantID)
	require.Equal(t, "r-01jsource", source.RunID)
	require.Equal(t, "codex", source.Harness)
	require.True(t, source.GrantExpiresAt.Equal(expiresAt))
	require.Nil(t, source.GrantRevokedAt)

	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/grants/"+grant.ID+"/revoke", nil, nil))
	workspace, err = env.member.Workspace(ctx, created.WorkspaceID)
	require.NoError(t, err)
	require.NotNil(t, workspace.Cooplay.Source.GrantRevokedAt)

	// Workspaces created in Coder carry nothing.
	plain := env.waitingWorkspace(t, "small")
	workspace, err = env.member.Workspace(ctx, plain.ID)
	require.NoError(t, err)
	require.Nil(t, workspace.Cooplay)
}

// TestWatchWakesOnWorkspaceState checks that a stored state ends a pending
// watch long poll.
func TestWatchWakesOnWorkspaceState(t *testing.T) {
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

	st := admissionapi.WorkspaceCooplayState{PowerState: codersdk.WorkspaceCooplayPowerPaused, PowerEpoch: 1, Since: dbtime.Now()}
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, "/v1/workspaces/"+ws.Workspace.ID.String()+"/state", st, nil))
	resp := testutil.TryReceive(ctx, t, done)
	require.True(t, resp.Changed)
	require.NotEqual(t, initial.Cursor, resp.Cursor)
}
