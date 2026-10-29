package cli_test

import (
	"bytes"
	"context"
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
