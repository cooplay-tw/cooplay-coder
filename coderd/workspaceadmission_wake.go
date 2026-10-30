package coderd

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/coderd/httpmw"
	"github.com/coder/coder/v2/coderd/rbac"
	"github.com/coder/coder/v2/coderd/rbac/policy"
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

// postWorkspaceAgentWake asks the platform controller to wake the workspace
// of an agent whose microVM may be asleep. CLI clients call it when they
// find the agent disconnected, because they wait for the agent to reconnect
// before they open a tunnel that would record a wake intent. The intent is
// written before the answer. It never starts a stopped workspace.
//
// @Summary Wake the workspace of an agent
// @ID wake-the-workspace-of-an-agent
// @Security CoderSessionToken
// @Produce json
// @Tags Agents
// @Param workspaceagent path string true "Workspace agent ID" format(uuid)
// @Success 202 {object} codersdk.WorkspaceAgentWakeResponse
// @Router /api/v2/workspaceagents/{workspaceagent}/wake [post]
func (api *API) postWorkspaceAgentWake(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	waws := httpmw.WorkspaceAgentAndWorkspaceParam(r)
	// Waking is part of connecting to the agent, so it needs the same
	// permission as the coordinate route.
	if !api.Authorize(r, policy.ActionSSH, waws) {
		httpapi.ResourceNotFound(rw)
		return
	}
	var result codersdk.WorkspaceAgentWakeResult
	switch api.wakeIntents.Record(waws.WorkspaceAgent.ID, wakeRequesterID(r), wakeintent.SourceCLI) {
	case wakeintent.ResultRecorded:
		result = codersdk.WorkspaceAgentWakeRequested
	case wakeintent.ResultSkippedRunning:
		result = codersdk.WorkspaceAgentWakeAwake
	case wakeintent.ResultSkippedUnmanaged:
		result = codersdk.WorkspaceAgentWakeNotManaged
	case wakeintent.ResultSkippedStopped:
		result = codersdk.WorkspaceAgentWakeNotStarted
	default:
		httpapi.Write(ctx, rw, http.StatusInternalServerError, codersdk.Response{
			Message: "Failed to record the wake request.",
		})
		return
	}
	httpapi.Write(ctx, rw, http.StatusAccepted, codersdk.WorkspaceAgentWakeResponse{Result: result})
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
