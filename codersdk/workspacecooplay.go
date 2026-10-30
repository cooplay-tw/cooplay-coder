package codersdk

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const (
	// WorkspaceCooplayKeepaliveDefaultSeconds is the keepalive duration of
	// a request without one: two hours, the "Keep awake 2 h" action.
	WorkspaceCooplayKeepaliveDefaultSeconds = 7200
	// WorkspaceCooplayKeepaliveMaxSeconds bounds a keepalive request. The
	// platform controller applies its own maximum.
	WorkspaceCooplayKeepaliveMaxSeconds = 43200
)

// WorkspaceCooplayKeepaliveRequest asks the platform controller to keep a
// workspace of a managed template awake.
type WorkspaceCooplayKeepaliveRequest struct {
	// ForSeconds is the requested keepalive duration. Zero requests
	// WorkspaceCooplayKeepaliveDefaultSeconds.
	ForSeconds int `json:"for_seconds,omitempty"`
}

// WorkspaceCooplayKeepaliveResponse is the recorded keepalive request. The
// platform controller applies it asynchronously.
type WorkspaceCooplayKeepaliveResponse struct {
	RequestedAt time.Time `json:"requested_at" format:"date-time"`
	ForSeconds  int       `json:"for_seconds"`
}

// WorkspaceAgentWakeResult tells what an explicit wake request did.
type WorkspaceAgentWakeResult string

const (
	// WorkspaceAgentWakeRequested means the platform controller was asked
	// to wake the workspace of the agent.
	WorkspaceAgentWakeRequested WorkspaceAgentWakeResult = "requested"
	// WorkspaceAgentWakeAwake means the workspace runs and its agent is
	// connected, so there was nothing to wake.
	WorkspaceAgentWakeAwake WorkspaceAgentWakeResult = "awake"
	// WorkspaceAgentWakeNotManaged means the template of the workspace is
	// not managed by workspace admission, so the platform never puts it to
	// sleep.
	WorkspaceAgentWakeNotManaged WorkspaceAgentWakeResult = "not_managed"
	// WorkspaceAgentWakeNotStarted means the workspace is stopped or
	// deleted, its latest build did not start it, or the agent belongs to
	// an older build. Start the workspace instead.
	WorkspaceAgentWakeNotStarted WorkspaceAgentWakeResult = "not_started"
)

// WorkspaceAgentWakeResponse is the answer to an explicit wake request.
type WorkspaceAgentWakeResponse struct {
	Result WorkspaceAgentWakeResult `json:"result" enums:"requested,awake,not_managed,not_started"`
}

// WorkspaceAgentWake asks the platform controller to wake the workspace of
// an agent whose microVM may be asleep. Clients call it when they find the
// agent disconnected, before they wait for it to reconnect. It needs SSH
// access to the workspace and never starts a stopped workspace.
func (c *Client) WorkspaceAgentWake(ctx context.Context, agentID uuid.UUID) (WorkspaceAgentWakeResponse, error) {
	res, err := c.Request(ctx, http.MethodPost, fmt.Sprintf("/api/v2/workspaceagents/%s/wake", agentID), nil)
	if err != nil {
		return WorkspaceAgentWakeResponse{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		return WorkspaceAgentWakeResponse{}, ReadBodyAsError(res)
	}
	var resp WorkspaceAgentWakeResponse
	return resp, ReadBodyAsJSON(res, &resp)
}

// WorkspaceCooplayKeepalive asks the platform controller to keep a workspace
// of a managed template awake.
func (c *Client) WorkspaceCooplayKeepalive(ctx context.Context, workspaceID uuid.UUID, req WorkspaceCooplayKeepaliveRequest) (WorkspaceCooplayKeepaliveResponse, error) {
	res, err := c.Request(ctx, http.MethodPost, fmt.Sprintf("/api/v2/workspaces/%s/cooplay/keepalive", workspaceID), req)
	if err != nil {
		return WorkspaceCooplayKeepaliveResponse{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		return WorkspaceCooplayKeepaliveResponse{}, ReadBodyAsError(res)
	}
	var resp WorkspaceCooplayKeepaliveResponse
	return resp, ReadBodyAsJSON(res, &resp)
}
