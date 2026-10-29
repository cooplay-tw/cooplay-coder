package coderd

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/coderd/httpmw"
	"github.com/coder/coder/v2/coderd/rbac/policy"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/codersdk"
)

// postWorkspaceCooplayKeepalive records a keepalive intent for a workspace of
// a managed template, for example from the "Keep awake 2 h" action of a
// pending sleep notice. The platform controller picks it up through the
// workspace admission service, extends the workspace keepalive and
// acknowledges it. A newer request replaces an unacknowledged one.
//
// @Summary Request a keepalive for a managed workspace
// @ID request-a-keepalive-for-a-managed-workspace
// @Security CoderSessionToken
// @Accept json
// @Produce json
// @Tags Workspaces
// @Param workspace path string true "Workspace ID" format(uuid)
// @Param request body codersdk.WorkspaceCooplayKeepaliveRequest false "Keepalive request"
// @Success 202 {object} codersdk.WorkspaceCooplayKeepaliveResponse
// @Router /api/v2/workspaces/{workspace}/cooplay/keepalive [post]
func (api *API) postWorkspaceCooplayKeepalive(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspace := httpmw.WorkspaceParam(r)
	apiKey := httpmw.APIKey(r)
	if !api.Authorize(r, policy.ActionUpdate, workspace) {
		httpapi.Forbidden(rw)
		return
	}

	// The body is optional: the workspace page sends none.
	var req codersdk.WorkspaceCooplayKeepaliveRequest
	if r.ContentLength != 0 {
		if !httpapi.Read(ctx, rw, r, &req) {
			return
		}
	}
	forSeconds := req.ForSeconds
	if forSeconds == 0 {
		forSeconds = codersdk.WorkspaceCooplayKeepaliveDefaultSeconds
	}
	if forSeconds < 1 || forSeconds > codersdk.WorkspaceCooplayKeepaliveMaxSeconds {
		httpapi.Write(ctx, rw, http.StatusBadRequest, codersdk.Response{
			Message: "Invalid keepalive duration.",
			Validations: []codersdk.ValidationError{{
				Field:  "for_seconds",
				Detail: fmt.Sprintf("Must be between 1 and %d seconds.", codersdk.WorkspaceCooplayKeepaliveMaxSeconds),
			}},
		})
		return
	}
	if workspace.Deleted {
		httpapi.Write(ctx, rw, http.StatusGone, codersdk.Response{
			Message: "Workspace is deleted.",
		})
		return
	}

	//nolint:gocritic // The requester may update the workspace (checked above); this subject only reads managed templates and writes intents.
	recorderCtx := dbauthz.AsWorkspaceWakeRecorder(ctx)
	if _, err := api.Database.GetWorkspaceAdmissionTemplateByTemplateID(recorderCtx, workspace.TemplateID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpapi.Write(ctx, rw, http.StatusBadRequest, codersdk.Response{
				Message: "Keepalives are only available for workspaces of managed templates.",
			})
			return
		}
		httpapi.InternalServerError(rw, err)
		return
	}
	intent, err := api.Database.UpsertWorkspaceAdmissionKeepaliveIntent(recorderCtx, database.UpsertWorkspaceAdmissionKeepaliveIntentParams{
		WorkspaceID:    workspace.ID,
		OrganizationID: workspace.OrganizationID,
		RequestedBy:    apiKey.UserID,
		RequestedAt:    dbtime.Time(api.Clock.Now()),
		ForSeconds:     int32(forSeconds), //nolint:gosec // Bounded by WorkspaceCooplayKeepaliveMaxSeconds.
	})
	if err != nil {
		httpapi.InternalServerError(rw, err)
		return
	}
	if err := workspaceadmission.PublishIntent(api.Pubsub, workspaceadmission.EventKindKeepaliveIntent, workspace.ID); err != nil {
		// The admission service watchers also poll.
		api.Logger.Warn(ctx, "publish workspace keepalive intent", slog.F("workspace_id", workspace.ID), slog.Error(err))
	}
	httpapi.Write(ctx, rw, http.StatusAccepted, codersdk.WorkspaceCooplayKeepaliveResponse{
		RequestedAt: intent.RequestedAt,
		ForSeconds:  int(intent.ForSeconds),
	})
}
