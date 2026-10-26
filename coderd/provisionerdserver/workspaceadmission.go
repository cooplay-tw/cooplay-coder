package provisionerdserver

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"

	"github.com/coder/coder/v2/coderd/database"
)

// withholdsOwnerTokens reports whether builds of a template must not receive
// the workspace owner's session token or OIDC access token. Templates managed
// by workspace admission start through a logical workspace provider that
// talks to the capacity controller; owner credentials must never reach it,
// the guest or the Terraform state, so their builds get neither token.
func withholdsOwnerTokens(ctx context.Context, db database.Store, templateID uuid.UUID) (bool, error) {
	_, err := db.GetWorkspaceAdmissionTemplateByTemplateID(ctx, templateID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
