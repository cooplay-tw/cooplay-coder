package agentsocket_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/agent/agentsocket"
	"github.com/coder/coder/v2/testutil"
)

type fakeSleepWakeHooks struct {
	wakes    atomic.Int32
	prepares atomic.Int32
	wakeErr  error
}

func (f *fakeSleepWakeHooks) PrepareSleep(context.Context) error {
	f.prepares.Add(1)
	return nil
}

func (f *fakeSleepWakeHooks) Wake(context.Context) error {
	f.wakes.Add(1)
	return f.wakeErr
}

func TestSleepWakeRPC(t *testing.T) {
	t.Parallel()

	t.Run("RoundTrip", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		socketPath := testutil.AgentSocketPath(t)
		hooks := &fakeSleepWakeHooks{}
		server, err := agentsocket.NewServer(slog.Make().Leveled(slog.LevelDebug),
			agentsocket.WithPath(socketPath),
			agentsocket.WithSleepWakeHooks(hooks),
		)
		require.NoError(t, err)
		defer server.Close()

		client, err := agentsocket.NewClient(ctx, agentsocket.WithPath(socketPath))
		require.NoError(t, err)
		defer client.Close()

		require.NoError(t, client.PrepareSleep(ctx))
		require.EqualValues(t, 1, hooks.prepares.Load())
		require.EqualValues(t, 0, hooks.wakes.Load())

		require.NoError(t, client.Wake(ctx))
		require.EqualValues(t, 1, hooks.wakes.Load())
	})

	t.Run("HookError", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		socketPath := testutil.AgentSocketPath(t)
		hooks := &fakeSleepWakeHooks{wakeErr: xerrors.New("boom")}
		server, err := agentsocket.NewServer(slog.Make().Leveled(slog.LevelDebug),
			agentsocket.WithPath(socketPath),
			agentsocket.WithSleepWakeHooks(hooks),
		)
		require.NoError(t, err)
		defer server.Close()

		client, err := agentsocket.NewClient(ctx, agentsocket.WithPath(socketPath))
		require.NoError(t, err)
		defer client.Close()

		require.ErrorContains(t, client.Wake(ctx), "boom")
	})

	t.Run("NoHooks", func(t *testing.T) {
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

		require.ErrorContains(t, client.Wake(ctx), "sleep/wake hooks not available")
		require.ErrorContains(t, client.PrepareSleep(ctx), "sleep/wake hooks not available")
	})
}
