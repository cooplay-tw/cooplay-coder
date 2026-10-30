package coderd

import (
	"context"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/db2sdk"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/codersdk"
)

// workspaceCooplay returns what the platform controller reported about the
// managed workspaces among workspaces that the request is already
// authorized to read, keyed by workspace ID.
//
// Like the admission of workspace builds (see workspaceBuildAdmissions),
// the reported states belong to the restricted admission service and no
// user role grants any permission on them. Every caller loads the
// workspaces through the requester's own authorization first, so this reads
// the rows of exactly those workspaces with a subject that can only read
// workspace admission. A row that cannot be decoded is left out and logged
// instead of failing the request.
func (api *API) workspaceCooplay(ctx context.Context, workspaces []database.Workspace) (map[uuid.UUID]codersdk.WorkspaceCooplay, error) {
	if len(workspaces) == 0 {
		return map[uuid.UUID]codersdk.WorkspaceCooplay{}, nil
	}
	ids := make([]uuid.UUID, 0, len(workspaces))
	for _, workspace := range workspaces {
		ids = append(ids, workspace.ID)
	}
	//nolint:gocritic // The requester is authorized to read these workspaces; see the function comment.
	rows, err := api.Database.GetWorkspaceAdmissionCooplayByWorkspaceIDs(dbauthz.AsWorkspaceAdmissionReader(ctx), ids)
	if err != nil {
		return nil, xerrors.Errorf("get workspace cooplay states: %w", err)
	}
	cooplay := make(map[uuid.UUID]codersdk.WorkspaceCooplay, len(rows))
	for _, row := range rows {
		converted, err := db2sdk.WorkspaceCooplay(row)
		if err != nil {
			api.Logger.Warn(ctx, "omit undecodable workspace cooplay state",
				slog.F("workspace_id", row.WorkspaceID),
				slog.Error(err),
			)
			continue
		}
		cooplay[row.WorkspaceID] = converted
	}
	return cooplay, nil
}
