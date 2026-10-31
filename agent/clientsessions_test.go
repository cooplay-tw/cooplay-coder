package agent_test

import (
	"context"
	"net"
	"reflect"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/agent"
	"github.com/coder/coder/v2/agent/agentsocket"
	"github.com/coder/coder/v2/agent/agentssh"
	"github.com/coder/coder/v2/agent/agenttest"
	"github.com/coder/coder/v2/codersdk/agentsdk"
	"github.com/coder/coder/v2/testutil"
)

// TestAgent_ClientSessions opens one client connection of every kind and reads
// the counts back through the agent socket, then closes them again.
func TestAgent_ClientSessions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the test uses a unix agent socket and a POSIX shell")
	}
	ctx := testutil.Context(t, testutil.WaitSuperLong)

	socketPath := testutil.AgentSocketPath(t)
	//nolint:dogsled
	conn, _, _, _, _ := setupAgent(t, agentsdk.Manifest{}, 0, func(_ *agenttest.Client, o *agent.Options) {
		o.SocketServerEnabled = true
		o.SocketPath = socketPath
	})
	socket, err := agentsocket.NewClient(ctx, agentsocket.WithPath(socketPath))
	require.NoError(t, err)
	t.Cleanup(func() { _ = socket.Close() })

	requireSessions := func(want agentsocket.Sessions) {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, testutil.WaitMedium)
		defer cancel()
		var got agentsocket.Sessions
		var gotErr error
		ok := testutil.Eventually(waitCtx, t, func(ctx context.Context) bool {
			sessions, err := socket.Sessions(ctx)
			if err != nil {
				gotErr = err
				return false
			}
			got = sessions
			return reflect.DeepEqual(got, want)
		}, testutil.IntervalFast)
		require.True(t, ok, "sessions: got %+v (err %v), want %+v", got, gotErr, want)
	}
	requireSessions(agentsocket.Sessions{SSH: map[string]int64{}})

	sshClient, err := conn.SSHClient(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sshClient.Close() })

	// SSH sessions are counted by app name while their shell runs.
	shell := func(appName string) {
		session, err := sshClient.NewSession()
		require.NoError(t, err)
		t.Cleanup(func() { _ = session.Close() })
		if appName != "" {
			require.NoError(t, session.Setenv(agentssh.AppNameEnvironmentVariable, appName))
		}
		_, err = session.StdinPipe()
		require.NoError(t, err)
		require.NoError(t, session.Shell())
	}
	shell("")
	shell("cursor")
	requireSessions(agentsocket.Sessions{SSH: map[string]int64{"ssh": 1, "cursor": 1}})

	pty, err := conn.ReconnectingPTY(ctx, uuid.New(), 80, 80, "sh")
	require.NoError(t, err)
	requireSessions(agentsocket.Sessions{SSH: map[string]int64{"ssh": 1, "cursor": 1}, ReconnectingPTY: 1})

	// A local service in the workspace, reached once through an SSH local
	// forward and once through the tailnet like `coder port-forward`.
	local, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })
	go func() {
		for {
			c, err := local.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	localAddr := local.Addr().String()

	sshForward, err := sshClient.Dial("tcp", localAddr)
	require.NoError(t, err)
	tailnetForward, err := conn.DialContext(ctx, "tcp", localAddr)
	require.NoError(t, err)
	requireSessions(agentsocket.Sessions{SSH: map[string]int64{"ssh": 1, "cursor": 1}, ReconnectingPTY: 1, PortForward: 2})

	require.NoError(t, sshForward.Close())
	require.NoError(t, tailnetForward.Close())
	require.NoError(t, pty.Close())
	requireSessions(agentsocket.Sessions{SSH: map[string]int64{"ssh": 1, "cursor": 1}})

	require.NoError(t, sshClient.Close())
	requireSessions(agentsocket.Sessions{SSH: map[string]int64{}})
}
