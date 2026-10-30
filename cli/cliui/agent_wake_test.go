package cliui_test

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/cli/cliui"
	"github.com/coder/coder/v2/coderd/util/ptr"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
)

// TestAgentWake checks that the waiter asks once to wake the workspace of
// an agent it finds disconnected or timed out, however often it polls the
// agent before it connects.
func TestAgentWake(t *testing.T) {
	t.Parallel()

	const wakingLine = "Waking the sleeping workspace."
	for _, tc := range []struct {
		name     string
		status   codersdk.WorkspaceAgentStatus
		result   codersdk.WorkspaceAgentWakeResult
		err      error
		wantLine bool
	}{
		{name: "Disconnected", status: codersdk.WorkspaceAgentDisconnected, result: codersdk.WorkspaceAgentWakeRequested, wantLine: true},
		{name: "Timeout", status: codersdk.WorkspaceAgentTimeout, result: codersdk.WorkspaceAgentWakeRequested, wantLine: true},
		{name: "Awake", status: codersdk.WorkspaceAgentDisconnected, result: codersdk.WorkspaceAgentWakeAwake},
		{name: "NotManaged", status: codersdk.WorkspaceAgentTimeout, result: codersdk.WorkspaceAgentWakeNotManaged},
		{name: "Error", status: codersdk.WorkspaceAgentDisconnected, err: xerrors.New("route not found")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := testutil.Context(t, testutil.WaitShort)
			agentID := uuid.New()
			past := time.Now().Add(-time.Minute)
			var fetches, wakes atomic.Int64
			var out bytes.Buffer
			err := cliui.Agent(ctx, &out, agentID, cliui.AgentOptions{
				FetchInterval: time.Millisecond,
				Fetch: func(_ context.Context, id uuid.UUID) (codersdk.WorkspaceAgent, error) {
					assert.Equal(t, agentID, id)
					agent := codersdk.WorkspaceAgent{
						ID:             id,
						Status:         tc.status,
						CreatedAt:      past,
						LifecycleState: codersdk.WorkspaceAgentLifecycleReady,
						StartedAt:      ptr.Ref(past),
						ReadyAt:        ptr.Ref(past),
					}
					if tc.status == codersdk.WorkspaceAgentDisconnected {
						agent.FirstConnectedAt = ptr.Ref(past)
						agent.LastConnectedAt = ptr.Ref(past)
						agent.DisconnectedAt = ptr.Ref(past)
					}
					// The agent connects after a few polls.
					if fetches.Add(1) > 5 {
						now := time.Now()
						agent.Status = codersdk.WorkspaceAgentConnected
						agent.FirstConnectedAt = ptr.Ref(past)
						agent.LastConnectedAt = ptr.Ref(now)
						agent.DisconnectedAt = nil
					}
					return agent, nil
				},
				Wake: func(_ context.Context, id uuid.UUID) (codersdk.WorkspaceAgentWakeResponse, error) {
					assert.Equal(t, agentID, id)
					wakes.Add(1)
					return codersdk.WorkspaceAgentWakeResponse{Result: tc.result}, tc.err
				},
			})
			require.NoError(t, err)
			require.Greater(t, fetches.Load(), int64(5))
			require.EqualValues(t, 1, wakes.Load())
			if tc.wantLine {
				require.Contains(t, out.String(), wakingLine)
			} else {
				require.NotContains(t, out.String(), wakingLine)
			}
		})
	}

	t.Run("ConnectedNeverWakes", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitShort)
		var wakes atomic.Int64
		err := cliui.Agent(ctx, &bytes.Buffer{}, uuid.New(), cliui.AgentOptions{
			FetchInterval: time.Millisecond,
			Fetch: func(_ context.Context, id uuid.UUID) (codersdk.WorkspaceAgent, error) {
				return codersdk.WorkspaceAgent{
					ID:             id,
					Status:         codersdk.WorkspaceAgentConnected,
					LifecycleState: codersdk.WorkspaceAgentLifecycleReady,
				}, nil
			},
			Wake: func(context.Context, uuid.UUID) (codersdk.WorkspaceAgentWakeResponse, error) {
				wakes.Add(1)
				return codersdk.WorkspaceAgentWakeResponse{Result: codersdk.WorkspaceAgentWakeRequested}, nil
			},
		})
		require.NoError(t, err)
		require.Zero(t, wakes.Load())
	})
}
