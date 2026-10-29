package agent

import (
	"context"
	"sync/atomic"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/agent/agentsocket"
	"github.com/coder/quartz"
	"github.com/coder/retry"
)

// Sleep/wake hooks for Cooplay agent workspaces.
//
// The workspace host (a Firecracker microVM) is suspended and restored by the
// platform controller while the Coder workspace build stays "running". The
// in-VM guest daemon drives the agent through the agent socket:
//
//   - PrepareSleep (before the VM is frozen and snapshotted) marks the current
//     and any future coderd connection so that tearing it down does NOT send
//     the graceful coordinator Disconnect. coderd then reports the agent as
//     LOST (not DISCONNECTED) once the frozen connection times out, and
//     clients keep the agent peer in their netmap (see
//     tailnet.DefaultLostTimeout) so they reconnect as soon as it is back.
//     PrepareSleep does not tear anything down by itself and does not affect
//     agent shutdown: a gracefully closing agent still sends Disconnect.
//   - Wake (after restore and the wall clock fix) clears PrepareSleep, closes
//     the current coderd RPC connection without the graceful Disconnect (or
//     aborts a dial still in progress), closes and redials DERP, rebinds the
//     tailnet sockets, pings known peers (see tailnet.Conn.Wake for when this
//     renews the WireGuard handshake) and makes runLoop redial immediately
//     with a reset backoff. Wake is safe to call at any time, including when
//     the agent is not connected (it then only interrupts the backoff wait).

var _ agentsocket.SleepWakeHooks = (*agent)(nil)

// DefaultReconnectStableThreshold is how long a coderd connection must stay
// up before runLoop resets its reconnect backoff to the initial delay.
const DefaultReconnectStableThreshold = 10 * time.Second

// rpcConnHandle tracks one coderd RPC connection established by run().
type rpcConnHandle struct {
	conn interface{ Close() error }
	// skipGracefulDisconnect makes runCoordinator close the coordinate stream
	// without sending Disconnect when this connection is torn down.
	skipGracefulDisconnect atomic.Bool
}

// startRunAttempt records the cancel func of a new run attempt. A wake that
// was signaled before the attempt started is satisfied by this attempt, so
// the pending signal is drained here (under the same lock Wake signals under)
// and does not skip a later, unrelated backoff wait.
func (a *agent) startRunAttempt(cancel context.CancelFunc) {
	a.rpcConnMu.Lock()
	defer a.rpcConnMu.Unlock()
	a.attemptCancel = cancel
	select {
	case <-a.wakeCh:
	default:
	}
}

func (a *agent) endRunAttempt() {
	a.rpcConnMu.Lock()
	defer a.rpcConnMu.Unlock()
	a.attemptCancel = nil
}

// registerRPCConn registers the connection dialed by the current attempt. It
// fails if Wake canceled the attempt while it was dialing, so a connection
// dialed before the wake is never used.
func (a *agent) registerRPCConn(attemptCtx context.Context, conn interface{ Close() error }) (*rpcConnHandle, error) {
	h := &rpcConnHandle{conn: conn}
	a.rpcConnMu.Lock()
	defer a.rpcConnMu.Unlock()
	if err := attemptCtx.Err(); err != nil {
		return nil, xerrors.Errorf("connection attempt canceled: %w", err)
	}
	if a.sleepPrepared {
		h.skipGracefulDisconnect.Store(true)
	}
	a.rpcConn = h
	return h, nil
}

func (a *agent) unregisterRPCConn(h *rpcConnHandle) {
	a.rpcConnMu.Lock()
	defer a.rpcConnMu.Unlock()
	if a.rpcConn == h {
		a.rpcConn = nil
	}
}

// PrepareSleep implements agentsocket.SleepWakeHooks.
func (a *agent) PrepareSleep(ctx context.Context) error {
	a.rpcConnMu.Lock()
	defer a.rpcConnMu.Unlock()
	a.sleepPrepared = true
	if a.rpcConn != nil {
		a.rpcConn.skipGracefulDisconnect.Store(true)
	}
	a.logger.Info(ctx, "prepared for sleep: connection teardown will skip the graceful coordinator disconnect")
	return nil
}

// Wake implements agentsocket.SleepWakeHooks.
func (a *agent) Wake(ctx context.Context) error {
	if a.isClosed() {
		return xerrors.New("agent is closed")
	}
	a.rpcConnMu.Lock()
	a.sleepPrepared = false
	h := a.rpcConn
	if h != nil {
		// The old connection must not send Disconnect while it is torn down;
		// the new connection starts without the flag.
		h.skipGracefulDisconnect.Store(true)
	}
	// Signal before tearing down so runLoop sees the wake as soon as run
	// returns. Signaling under rpcConnMu orders it against startRunAttempt.
	select {
	case a.wakeCh <- struct{}{}:
	default:
	}
	// Abort the current attempt, including a dial in progress that would
	// otherwise hang on a connection that died during the suspend.
	if a.attemptCancel != nil {
		a.attemptCancel()
	}
	a.rpcConnMu.Unlock()

	a.logger.Info(ctx, "waking: dropping coderd and DERP connections and reconnecting")

	if network, ok := a.requireNetwork(); ok {
		// Bound the handshake pings by the agent lifetime, not the caller's
		// request context.
		network.Wake(a.hardCtx)
	}

	if h != nil {
		if err := h.conn.Close(); err != nil {
			a.logger.Debug(ctx, "error closing coderd connection on wake", slog.Error(err))
		}
	}
	return nil
}

// reconnectBackoff wraps the runLoop retrier: it resets the delay after a
// connection stayed up for at least threshold, and skips the wait when woken.
type reconnectBackoff struct {
	retrier   *retry.Retrier
	clock     quartz.Clock
	threshold time.Duration
}

func newReconnectBackoff(clock quartz.Clock, threshold, floor, ceil time.Duration) *reconnectBackoff {
	return &reconnectBackoff{
		retrier:   retry.New(floor, ceil),
		clock:     clock,
		threshold: threshold,
	}
}

// connectionEnded records the end of a connection that was established at
// connectedAt (zero if it never connected).
func (b *reconnectBackoff) connectionEnded(connectedAt time.Time) {
	if connectedAt.IsZero() {
		return
	}
	if b.clock.Since(connectedAt) >= b.threshold {
		b.retrier.Reset()
	}
}

// wait blocks for the backoff delay, returning early (with a reset backoff)
// when wake is signaled. It returns false when ctx is done.
func (b *reconnectBackoff) wait(ctx context.Context, wake <-chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case <-wake:
		b.retrier.Reset()
		return true
	default:
	}

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var woke atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-wake:
			woke.Store(true)
			cancel()
		case <-waitCtx.Done():
		}
	}()
	ok := b.retrier.Wait(waitCtx)
	cancel()
	<-done
	if woke.Load() {
		b.retrier.Reset()
		return ctx.Err() == nil
	}
	return ok
}
