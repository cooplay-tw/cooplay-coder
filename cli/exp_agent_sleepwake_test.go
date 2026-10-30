package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/agent/agentsocket"
	"github.com/coder/coder/v2/cli/clitest"
	"github.com/coder/coder/v2/testutil"
)

type cliSleepWakeHooks struct {
	wakes    atomic.Int32
	prepares atomic.Int32
}

func (h *cliSleepWakeHooks) PrepareSleep(context.Context) error {
	h.prepares.Add(1)
	return nil
}

func (h *cliSleepWakeHooks) Wake(context.Context) error {
	h.wakes.Add(1)
	return nil
}

func TestExpAgentSleepWake(t *testing.T) {
	t.Parallel()

	socketPath := testutil.AgentSocketPath(t)
	hooks := &cliSleepWakeHooks{}
	server, err := agentsocket.NewServer(slog.Make().Leveled(slog.LevelDebug),
		agentsocket.WithPath(socketPath),
		agentsocket.WithSleepWakeHooks(hooks),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })

	ctx := testutil.Context(t, testutil.WaitShort)

	var out bytes.Buffer
	inv, _ := clitest.New(t, "exp", "agent", "prepare-sleep", "--socket-path", socketPath)
	inv.Stdout = &out
	require.NoError(t, inv.WithContext(ctx).Run())
	require.EqualValues(t, 1, hooks.prepares.Load())
	require.Contains(t, out.String(), "Success")

	out.Reset()
	inv, _ = clitest.New(t, "exp", "agent", "wake", "--socket-path", socketPath)
	inv.Stdout = &out
	require.NoError(t, inv.WithContext(ctx).Run())
	require.EqualValues(t, 1, hooks.wakes.Load())
	require.Contains(t, out.String(), "Success")
}

type cliSessionReporter struct {
	sessions agentsocket.Sessions
}

func (r cliSessionReporter) ClientSessions() agentsocket.Sessions {
	return r.sessions
}

// TestExpAgentSessions checks the output of `coder exp agent sessions`.
// testdata/agent-sessions.json is a byte-for-byte copy of the Cooplay platform
// golden file services/agent-workspaces/internal/api/testdata/agent-sessions.json
// (owned by the platform's protocol package, parsed strictly by its guest
// daemon); update both together.
func TestExpAgentSessions(t *testing.T) {
	t.Parallel()

	runSessions := func(t *testing.T, sessions agentsocket.Sessions, args ...string) string {
		t.Helper()
		socketPath := testutil.AgentSocketPath(t)
		server, err := agentsocket.NewServer(slog.Make().Leveled(slog.LevelDebug),
			agentsocket.WithPath(socketPath),
			agentsocket.WithSessionReporter(cliSessionReporter{sessions: sessions}),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = server.Close() })

		ctx := testutil.Context(t, testutil.WaitShort)
		var out bytes.Buffer
		inv, _ := clitest.New(t, append([]string{"exp", "agent", "sessions", "--socket-path", socketPath}, args...)...)
		inv.Stdout = &out
		require.NoError(t, inv.WithContext(ctx).Run())
		return out.String()
	}

	// One app name per family case: zed is in the SSH family, cursor in the
	// VS Code family, and unknown names and reconnecting_pty (which has no
	// key of its own among SSH sessions) are summed into other.
	golden := agentsocket.Sessions{
		SSH: map[string]int64{
			"zed":              1,
			"vscode":           1,
			"cursor":           1,
			"jetbrains":        3,
			"my_app":           3,
			"reconnecting_pty": 1,
			"ssh":              0,
		},
		ReconnectingPTY: 5,
		PortForward:     6,
	}

	t.Run("JSONGolden", func(t *testing.T) {
		t.Parallel()

		want, err := os.ReadFile(filepath.Join("testdata", "agent-sessions.json"))
		require.NoError(t, err)
		require.Equal(t, string(want), runSessions(t, golden, "--json"))
	})

	t.Run("JSONNoSessions", func(t *testing.T) {
		t.Parallel()

		got := runSessions(t, agentsocket.Sessions{SSH: map[string]int64{}}, "--json")
		require.Equal(t, `{"ssh":0,"vscode":0,"jetbrains":0,"other":0,"pty":0,"port_forward":0}`+"\n", got)
	})

	t.Run("JSONNeverNegative", func(t *testing.T) {
		t.Parallel()

		got := runSessions(t, agentsocket.Sessions{
			SSH:             map[string]int64{"ssh": -1},
			ReconnectingPTY: -2,
			PortForward:     -3,
		}, "--json")
		require.Equal(t, `{"ssh":0,"vscode":0,"jetbrains":0,"other":0,"pty":0,"port_forward":0}`+"\n", got)
	})

	t.Run("Table", func(t *testing.T) {
		t.Parallel()

		got := runSessions(t, golden)
		for _, line := range []string{
			"SSH           1",
			"VS Code       2",
			"JetBrains     3",
			"Other         4",
			"Web terminal  5",
			"Port forward  6",
		} {
			require.Contains(t, got, line+"\n")
		}
	})
}
