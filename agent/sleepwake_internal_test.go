package agent

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/tailnet"
	"github.com/coder/coder/v2/tailnet/proto"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

func TestReconnectBackoff_ResetAfterStableConnection(t *testing.T) {
	t.Parallel()

	mClock := quartz.NewMock(t)
	b := newReconnectBackoff(mClock, 10*time.Second, time.Millisecond, time.Hour)
	ctx := testutil.Context(t, testutil.WaitShort)
	wake := make(chan struct{}, 1)

	// Grow the backoff a few steps.
	for range 4 {
		require.True(t, b.wait(ctx, wake))
	}
	grown := b.retrier.Delay
	require.Greater(t, grown, time.Duration(0))

	// A connection that never came up does not reset.
	b.connectionEnded(time.Time{})
	require.Equal(t, grown, b.retrier.Delay)

	// An unstable connection (shorter than the threshold) does not reset.
	connectedAt := mClock.Now()
	mClock.Advance(9 * time.Second)
	b.connectionEnded(connectedAt)
	require.Equal(t, grown, b.retrier.Delay)

	// A stable connection resets to the initial delay.
	connectedAt = mClock.Now()
	mClock.Advance(10 * time.Second)
	b.connectionEnded(connectedAt)
	require.Equal(t, time.Duration(0), b.retrier.Delay)
}

func TestReconnectBackoff_WakeSkipsWait(t *testing.T) {
	t.Parallel()

	b := newReconnectBackoff(quartz.NewMock(t), 10*time.Second, time.Hour, time.Hour)
	ctx := testutil.Context(t, testutil.WaitShort)

	// Simulate a large pending delay.
	b.retrier.Delay = time.Hour

	// Wake signaled before wait: returns immediately and resets.
	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	require.True(t, b.wait(ctx, wake))
	require.Equal(t, time.Duration(0), b.retrier.Delay)

	// Wake signaled during wait interrupts the backoff.
	b.retrier.Delay = time.Hour
	go func() {
		time.Sleep(10 * time.Millisecond)
		wake <- struct{}{}
	}()
	require.True(t, b.wait(ctx, wake))
	require.Equal(t, time.Duration(0), b.retrier.Delay)

	// A canceled context stops the loop.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	require.False(t, b.wait(cctx, wake))
}

type fakeCloser struct {
	mu     sync.Mutex
	closed bool
}

func (f *fakeCloser) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeCloser) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func TestPrepareSleepAndWake_ConnHandles(t *testing.T) {
	t.Parallel()

	ctx := testutil.Context(t, testutil.WaitShort)
	a := &agent{logger: testutil.Logger(t), wakeCh: make(chan struct{}, 1)}
	a.hardCtx, a.hardCancel = context.WithCancel(context.Background())
	defer a.hardCancel()

	c1 := &fakeCloser{}
	h1, err := a.registerRPCConn(ctx, c1)
	require.NoError(t, err)
	require.False(t, h1.skipGracefulDisconnect.Load())

	require.NoError(t, a.PrepareSleep(ctx))
	require.True(t, h1.skipGracefulDisconnect.Load())

	// A connection made while sleep is prepared inherits the flag.
	a.unregisterRPCConn(h1)
	c2 := &fakeCloser{}
	h2, err := a.registerRPCConn(ctx, c2)
	require.NoError(t, err)
	require.True(t, h2.skipGracefulDisconnect.Load())

	// Wake keeps the flag on the old connection, closes it, signals the
	// run loop and clears PrepareSleep for new connections.
	require.NoError(t, a.Wake(ctx))
	require.True(t, h2.skipGracefulDisconnect.Load())
	require.True(t, c2.isClosed())
	select {
	case <-a.wakeCh:
	default:
		t.Fatal("wake not signaled")
	}
	a.unregisterRPCConn(h2)
	h3, err := a.registerRPCConn(ctx, &fakeCloser{})
	require.NoError(t, err)
	require.False(t, h3.skipGracefulDisconnect.Load())

	// Wake while connected but not prepared still drops the connection
	// without a graceful disconnect.
	require.NoError(t, a.Wake(ctx))
	require.True(t, h3.skipGracefulDisconnect.Load())
}

// TestWake_CancelsDialInProgress verifies that Wake aborts a run attempt that
// is still dialing (no connection registered yet), that the connection such an
// attempt might still produce is refused, and that a wake signaled before an
// attempt starts does not leak into a later backoff wait.
func TestWake_CancelsDialInProgress(t *testing.T) {
	t.Parallel()

	ctx := testutil.Context(t, testutil.WaitShort)
	a := &agent{logger: testutil.Logger(t), wakeCh: make(chan struct{}, 1)}
	a.hardCtx, a.hardCancel = context.WithCancel(context.Background())
	defer a.hardCancel()

	attemptCtx, attemptCancel := context.WithCancel(a.hardCtx)
	defer attemptCancel()
	a.startRunAttempt(attemptCancel)

	require.NoError(t, a.Wake(ctx))
	require.ErrorIs(t, attemptCtx.Err(), context.Canceled, "wake must cancel the dial in progress")
	c := &fakeCloser{}
	_, err := a.registerRPCConn(attemptCtx, c)
	require.Error(t, err, "a connection dialed before the wake must not be registered")
	a.endRunAttempt()
	select {
	case <-a.wakeCh:
	default:
		t.Fatal("wake not signaled")
	}

	// A wake signaled between attempts is consumed by the next attempt.
	require.NoError(t, a.Wake(ctx))
	nextCtx, nextCancel := context.WithCancel(a.hardCtx)
	defer nextCancel()
	a.startRunAttempt(nextCancel)
	require.NoError(t, nextCtx.Err())
	select {
	case <-a.wakeCh:
		t.Fatal("stale wake signal survived the start of a new attempt")
	default:
	}
	a.endRunAttempt()
}

// fakeCoordClient is a tailnet.CoordinatorClient that records requests and
// hangs up after a Disconnect, like a real coordinator.
type fakeCoordClient struct {
	mu         sync.Mutex
	reqs       []*proto.CoordinateRequest
	closed     bool
	hangup     chan struct{}
	hangupOnce sync.Once
}

func newFakeCoordClient() *fakeCoordClient {
	return &fakeCoordClient{hangup: make(chan struct{})}
}

func (f *fakeCoordClient) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.hangupOnce.Do(func() { close(f.hangup) })
	return nil
}

func (f *fakeCoordClient) Send(req *proto.CoordinateRequest) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if req.GetDisconnect() != nil {
		f.hangupOnce.Do(func() { close(f.hangup) })
	}
	return nil
}

func (f *fakeCoordClient) Recv() (*proto.CoordinateResponse, error) {
	<-f.hangup
	return nil, io.EOF
}

func (f *fakeCoordClient) sentDisconnect() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r.GetDisconnect() != nil {
			return true
		}
	}
	return false
}

func (f *fakeCoordClient) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

type fakeCoordinatee struct{}

func (fakeCoordinatee) UpdatePeers([]*proto.CoordinateResponse_PeerUpdate) error { return nil }
func (fakeCoordinatee) SetAllPeersLost()                                         {}
func (fakeCoordinatee) SetNodeCallback(func(*tailnet.Node))                      {}
func (fakeCoordinatee) SetTunnelDestination(uuid.UUID)                           {}

func TestCloseCoordination_SkipsGracefulDisconnect(t *testing.T) {
	t.Parallel()

	newAgent := func(t *testing.T) *agent {
		a := &agent{logger: testutil.Logger(t)}
		a.hardCtx, a.hardCancel = context.WithCancel(context.Background())
		a.gracefulCtx, a.gracefulCancel = context.WithCancel(a.hardCtx)
		t.Cleanup(a.hardCancel)
		return a
	}

	run := func(t *testing.T, a *agent, h *rpcConnHandle) *fakeCoordClient {
		ctx := testutil.Context(t, testutil.WaitShort)
		client := newFakeCoordClient()
		coordination := tailnet.NewAgentCoordinationController(a.logger, fakeCoordinatee{}).New(client)
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.closeCoordination(ctx, h, coordination, client)
		}()
		testutil.TryReceive(ctx, t, done)
		return client
	}

	t.Run("Normal", func(t *testing.T) {
		t.Parallel()
		a := newAgent(t)
		client := run(t, a, &rpcConnHandle{})
		require.True(t, client.sentDisconnect())
	})

	t.Run("PrepareSleep", func(t *testing.T) {
		t.Parallel()
		a := newAgent(t)
		h := &rpcConnHandle{}
		h.skipGracefulDisconnect.Store(true)
		client := run(t, a, h)
		require.False(t, client.sentDisconnect())
		require.True(t, client.isClosed())
	})

	t.Run("ShutdownStillGraceful", func(t *testing.T) {
		t.Parallel()
		a := newAgent(t)
		a.gracefulCancel()
		h := &rpcConnHandle{}
		h.skipGracefulDisconnect.Store(true)
		client := run(t, a, h)
		require.True(t, client.sentDisconnect())
	})
}
