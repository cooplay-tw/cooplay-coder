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
