package coderd

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/rbac"
	"github.com/coder/coder/v2/coderd/workspaceadmission/wakeintent"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/tailnet"
)

// wakeIntentAuthorizer records a wake intent for every tunnel that the inner
// authorizer allows, so a client that connects to an agent of a sleeping
// managed workspace wakes it. AuthorizeTunnel runs with the tailnet
// coordinator mutex held, so it only enqueues; the recorder does the
// database work. Coder Connect (Coder Desktop) opens tunnels to every agent
// of the user and must never wake workspaces, so its connections only count
// the skipped requests.
type wakeIntentAuthorizer struct {
	inner    tailnet.TunnelAuthorizer
	recorder *wakeintent.Recorder
	userID   uuid.UUID
	connect  atomic.Bool
}

var (
	_ tailnet.TunnelAuthorizer         = (*wakeIntentAuthorizer)(nil)
	_ tailnet.WorkspaceUpdatesObserver = (*wakeIntentAuthorizer)(nil)
)

// newWakeIntentAuthorizer wraps inner for the tailnet connection of r. A
// connection that carries the Coder Desktop telemetry header is Coder
// Connect from the start.
func newWakeIntentAuthorizer(inner tailnet.TunnelAuthorizer, recorder *wakeintent.Recorder, userID uuid.UUID, r *http.Request) *wakeIntentAuthorizer {
	a := &wakeIntentAuthorizer{inner: inner, recorder: recorder, userID: userID}
	if r.Header.Get(codersdk.CoderDesktopTelemetryHeader) != "" {
		a.connect.Store(true)
	}
	return a
}

func (a *wakeIntentAuthorizer) AuthorizeTunnel(ctx context.Context, agentID uuid.UUID) error {
	if err := a.inner.AuthorizeTunnel(ctx, agentID); err != nil {
		return err
	}
	if a.connect.Load() {
		a.recorder.SkipConnect(wakeintent.SourceTailnet)
		return nil
	}
	a.recorder.Enqueue(agentID, a.userID, wakeintent.SourceTailnet)
	return nil
}

// ObserveWorkspaceUpdates marks the connection as Coder Connect. Only Coder
// Connect subscribes to workspace updates, and it does so before it learns
// the agents it opens tunnels to.
func (a *wakeIntentAuthorizer) ObserveWorkspaceUpdates() {
	a.connect.Store(true)
}

// wakeRequesterID returns the user making the request, or zero when the
// request is not made by a user, for example by a workspace proxy.
func wakeRequesterID(r *http.Request) uuid.UUID {
	actor, ok := dbauthz.ActorFromContext(r.Context())
	if !ok || actor.Type != rbac.SubjectTypeUser {
		return uuid.Nil
	}
	id, err := uuid.Parse(actor.ID)
	if err != nil {
		return uuid.Nil
	}
	return id
}
