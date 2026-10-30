package agentsocket_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/agent/agentsocket"
	"github.com/coder/coder/v2/testutil"
)

type fakeSessionReporter struct {
	sessions agentsocket.Sessions
}

func (f fakeSessionReporter) ClientSessions() agentsocket.Sessions {
	return f.sessions
}

func TestSessionsRPC(t *testing.T) {
	t.Parallel()

	t.Run("RoundTrip", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		socketPath := testutil.AgentSocketPath(t)
		want := agentsocket.Sessions{
			SSH:             map[string]int64{"ssh": 2, "vscode": 1, "my_app": 3},
			ReconnectingPTY: 4,
			PortForward:     5,
		}
		server, err := agentsocket.NewServer(slog.Make().Leveled(slog.LevelDebug),
			agentsocket.WithPath(socketPath),
			agentsocket.WithSessionReporter(fakeSessionReporter{sessions: want}),
		)
		require.NoError(t, err)
		defer server.Close()

		client, err := agentsocket.NewClient(ctx, agentsocket.WithPath(socketPath))
		require.NoError(t, err)
		defer client.Close()

		got, err := client.Sessions(ctx)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("NoSessions", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		socketPath := testutil.AgentSocketPath(t)
		server, err := agentsocket.NewServer(slog.Make().Leveled(slog.LevelDebug),
			agentsocket.WithPath(socketPath),
			agentsocket.WithSessionReporter(fakeSessionReporter{}),
		)
		require.NoError(t, err)
		defer server.Close()

		client, err := agentsocket.NewClient(ctx, agentsocket.WithPath(socketPath))
		require.NoError(t, err)
		defer client.Close()

		got, err := client.Sessions(ctx)
		require.NoError(t, err)
		require.NotNil(t, got.SSH, "the SSH map is never nil")
		require.Equal(t, agentsocket.Sessions{SSH: map[string]int64{}}, got)
	})

	t.Run("NoReporter", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		socketPath := testutil.AgentSocketPath(t)
		server, err := agentsocket.NewServer(slog.Make().Leveled(slog.LevelDebug),
			agentsocket.WithPath(socketPath),
		)
		require.NoError(t, err)
		defer server.Close()

		client, err := agentsocket.NewClient(ctx, agentsocket.WithPath(socketPath))
		require.NoError(t, err)
		defer client.Close()

		_, err = client.Sessions(ctx)
		require.ErrorContains(t, err, "session reporter not available")
	})
}
