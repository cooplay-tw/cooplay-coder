package workspaceadmission

import (
	"context"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/codersdk"
)

// ErrOwnerInactive is returned when a workspace would be created on behalf
// of an owner that is not an active user: suspended, dormant, deleted or a
// system user.
var ErrOwnerInactive = xerrors.New("the workspace owner is not an active user")

// CreateWorkspaceParams describes a workspace that the admission service
// creates on behalf of its owner. The workspace, its START build and the
// waiting admission of the build commit in one transaction, the same one
// that runs the hooks.
type CreateWorkspaceParams struct {
	// OwnerID owns the new workspace. The workspace is created with the
	// owner's permissions, and the owner initiates its START build.
	OwnerID uuid.UUID
	Request codersdk.CreateWorkspaceRequest
	// PreCreateInTX runs in the create transaction before the workspace is
	// inserted. An error aborts the create and is returned as is, wrapped.
	PreCreateInTX func(ctx context.Context, tx database.Store) error
	// PostBuildInTX runs in the create transaction after the workspace, its
	// START build and the build's admission were inserted. An error aborts
	// the create and is returned as is, wrapped.
	PostBuildInTX func(ctx context.Context, tx database.Store, workspace database.Workspace, build database.WorkspaceBuild, job database.ProvisionerJob) error
}

// CreateWorkspaceFunc creates a workspace and its START build on behalf of
// its owner. It returns ErrOwnerInactive for owners that are not active.
type CreateWorkspaceFunc func(ctx context.Context, params CreateWorkspaceParams) (codersdk.Workspace, error)
