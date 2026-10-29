package coderd

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/coderd/audit"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/httpapi/httperror"
	"github.com/coder/coder/v2/coderd/httpmw"
	"github.com/coder/coder/v2/coderd/rbac"
	"github.com/coder/coder/v2/coderd/tracing"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/codersdk"
)

// CreateWorkspaceForAdmission creates a workspace and its START build on
// behalf of the owner for the restricted workspace admission service. The
// create runs with the owner's own permissions and the owner initiates the
// build, so organization membership, template access, the owner's
// entitlement and the waiting admission all apply exactly as when the owner
// creates the workspace. Owners that are not active are rejected with
// workspaceadmission.ErrOwnerInactive, also when they become inactive while
// the create runs. The hooks of params run in the create transaction.
func (api *API) CreateWorkspaceForAdmission(ctx context.Context, params workspaceadmission.CreateWorkspaceParams) (codersdk.Workspace, error) {
	owner, err := activeWorkspaceOwner(ctx, api.Database, params.OwnerID)
	if err != nil {
		return codersdk.Workspace{}, err
	}
	actor, _, err := httpmw.UserRBACSubject(ctx, api.Database, owner.ID, rbac.ScopeAll)
	if err != nil {
		return codersdk.Workspace{}, xerrors.Errorf("load workspace owner authorization: %w", err)
	}
	ctx = dbauthz.As(ctx, actor)

	auditor := api.Auditor.Load()
	if auditor == nil {
		return codersdk.Workspace{}, xerrors.New("auditor is not configured")
	}
	// The audit system needs a response writer and a request; this create
	// has neither, as in chatCreateWorkspace.
	sw := &tracing.StatusWriter{ResponseWriter: httptest.NewRecorder()}
	auditHTTPReq, err := http.NewRequestWithContext(
		httpmw.WithRequestID(ctx, uuid.New()),
		http.MethodPost,
		"http://localhost/internal/workspace-admission/workspaces",
		nil,
	)
	if err != nil {
		return codersdk.Workspace{}, xerrors.Errorf("create audit request: %w", err)
	}
	aReq, commitAudit := audit.InitRequest[database.WorkspaceTable](sw, &audit.RequestParams{
		Audit:   *auditor,
		Log:     api.Logger,
		Request: auditHTTPReq,
		Action:  database.AuditActionCreate,
		AdditionalFields: audit.AdditionalFields{
			WorkspaceOwner: owner.Username,
		},
	})
	aReq.UserID = owner.ID
	defer commitAudit()

	workspace, err := createWorkspace(ctx, aReq, owner.ID, api, workspaceOwner{
		ID:        owner.ID,
		Username:  owner.Username,
		AvatarURL: owner.AvatarURL,
	}, params.Request, &createWorkspaceOptions{
		preCreateInTX: func(ctx context.Context, tx database.Store) error {
			// The owner may have been suspended since the check above.
			if _, err := activeWorkspaceOwner(ctx, tx, owner.ID); err != nil {
				return err
			}
			if params.PreCreateInTX != nil {
				return params.PreCreateInTX(ctx, tx)
			}
			return nil
		},
		postBuildInTX: params.PostBuildInTX,
	})
	if err != nil {
		sw.WriteHeader(chatWorkspaceAuditStatus(err))
		return codersdk.Workspace{}, err
	}
	sw.WriteHeader(http.StatusCreated)
	return workspace, nil
}

// activeWorkspaceOwner returns the user that a workspace is created for on
// behalf of the admission service, or workspaceadmission.ErrOwnerInactive
// when that user may not own new workspaces.
func activeWorkspaceOwner(ctx context.Context, db database.Store, ownerID uuid.UUID) (database.User, error) {
	//nolint:gocritic // The admission service decides whom to create for; this only reads the owner's status.
	user, err := db.GetUserByID(dbauthz.AsSystemRestricted(ctx), ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return database.User{}, httperror.NewResponseError(http.StatusNotFound, codersdk.Response{
			Message: "Workspace owner not found.",
		})
	}
	if err != nil {
		return database.User{}, xerrors.Errorf("get workspace owner: %w", err)
	}
	if user.Deleted || user.IsSystem || user.Status != database.UserStatusActive {
		return database.User{}, workspaceadmission.ErrOwnerInactive
	}
	return user, nil
}
