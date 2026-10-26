package coderd

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
)

// workspaceBuildAdmissions returns the workspace admissions of builds that the
// request is already authorized to read.
//
// Admission records belong to the restricted admission service, so no user
// role grants any permission on them. The admission of a build is part of the
// build state, though, and whoever may read the build may see why it waits.
// Every caller loads the builds through the requester's own authorization
// first (the workspace or build route parameter, or an authorized workspace
// query), so this reads the rows of exactly those builds with a subject that
// can only read admissions. It never widens what the requester can see.
func (api *API) workspaceBuildAdmissions(ctx context.Context, builds []database.WorkspaceBuild) ([]database.WorkspaceBuildAdmission, error) {
	buildIDs := make([]uuid.UUID, 0, len(builds))
	for _, build := range builds {
		// Only START builds of managed templates have an admission.
		if build.Transition == database.WorkspaceTransitionStart {
			buildIDs = append(buildIDs, build.ID)
		}
	}
	if len(buildIDs) == 0 {
		return nil, nil
	}

	//nolint:gocritic // The requester is authorized to read these builds; see the function comment.
	admissions, err := api.Database.GetWorkspaceBuildAdmissionsByBuildIDs(dbauthz.AsWorkspaceAdmissionReader(ctx), buildIDs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, xerrors.Errorf("get workspace build admissions: %w", err)
	}
	return admissions, nil
}

// createdWorkspaceBuildAdmissions returns the admission of a build that the
// requester just created. The build is already committed, so failing to read
// its admission only omits it from the response instead of failing a request
// whose build exists.
func (api *API) createdWorkspaceBuildAdmissions(ctx context.Context, build database.WorkspaceBuild) []database.WorkspaceBuildAdmission {
	admissions, err := api.workspaceBuildAdmissions(ctx, []database.WorkspaceBuild{build})
	if err != nil {
		api.Logger.Warn(ctx, "failed to read the admission of a created workspace build",
			slog.F("workspace_build_id", build.ID),
			slog.Error(err),
		)
		return nil
	}
	return admissions
}
