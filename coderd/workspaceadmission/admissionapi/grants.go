package admissionapi

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/httpapi"
)

// maxGrantWorkspaces bounds GrantMirror.MaxWorkspaces.
const maxGrantWorkspaces = 1000

func grantIDParam(r *http.Request) (string, error) {
	id := chi.URLParam(r, "grant_id")
	if !controllerIDPattern.MatchString(id) {
		return "", newAPIError(http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Invalid grant_id: it must match %s.", controllerIDPattern))
	}
	return id, nil
}

// putGrant serves PUT /v1/grants/{grant_id}: it mirrors the scope of a
// controller grant. The owner and organization of a grant never change; a
// mirror that names others fails with operation_conflict. A revocation is
// final, so a later mirror never clears it.
func (s *Server) putGrant(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	grantID, err := grantIDParam(r)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body GrantMirror
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	invalid := func(msg string) {
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, msg, "")
	}
	switch {
	case body.ID != "" && body.ID != grantID:
		invalid("Field id must match the grant in the path.")
		return
	case body.OwnerID == uuid.Nil:
		invalid("Field owner_id is required.")
		return
	case body.OrganizationID == uuid.Nil:
		invalid("Field organization_id is required.")
		return
	case body.MaxWorkspaces < 0 || body.MaxWorkspaces > maxGrantWorkspaces:
		invalid(fmt.Sprintf("Field max_workspaces must be between 0 and %d.", maxGrantWorkspaces))
		return
	case len(body.NamePrefix) > maxNamePrefixLength:
		invalid(fmt.Sprintf("Field name_prefix is at most %d bytes long.", maxNamePrefixLength))
		return
	case body.ExpiresAt.IsZero():
		invalid("Field expires_at is required.")
		return
	case body.RevokedAt != nil && body.RevokedAt.IsZero():
		invalid("Field revoked_at must be a time when set.")
		return
	}
	templateIDs := make([]uuid.UUID, 0, len(body.TemplateIDs))
	for _, id := range body.TemplateIDs {
		if id == uuid.Nil {
			invalid("Field template_ids must not contain the nil UUID.")
			return
		}
		templateIDs = append(templateIDs, id)
	}
	if _, err := s.db.GetUserByID(ctx, body.OwnerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(ctx, rw, http.StatusNotFound, CodeNotFound, "Owner not found.", "")
			return
		}
		s.writeErr(ctx, rw, xerrors.Errorf("get owner: %w", err))
		return
	}
	revokedAt := sql.NullTime{}
	if body.RevokedAt != nil {
		revokedAt = sql.NullTime{Time: *body.RevokedAt, Valid: true}
	}
	row, err := s.db.UpsertWorkspaceAdmissionGrant(ctx, database.UpsertWorkspaceAdmissionGrantParams{
		ID:             grantID,
		OwnerID:        body.OwnerID,
		OrganizationID: body.OrganizationID,
		TemplateIDs:    templateIDs,
		NamePrefix:     body.NamePrefix,
		MaxWorkspaces:  int32(body.MaxWorkspaces), //nolint:gosec // Bounded by maxGrantWorkspaces.
		AllowDelete:    body.AllowDelete,
		ExpiresAt:      body.ExpiresAt,
		RevokedAt:      revokedAt,
		Now:            dbtime.Now(),
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(ctx, rw, http.StatusConflict, CodeOperationConflict, "The grant belongs to another owner or organization.", "")
		return
	case database.IsForeignKeyViolation(err):
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field organization_id does not name an organization.", "")
		return
	case err != nil:
		s.writeErr(ctx, rw, xerrors.Errorf("mirror grant: %w", err))
		return
	}
	s.logger.Info(ctx, "workspace admission mirrored a grant",
		slog.F("grant_id", row.ID),
		slog.F("owner_id", row.OwnerID),
		slog.F("max_workspaces", row.MaxWorkspaces),
		slog.F("revoked", row.RevokedAt.Valid),
	)
	httpapi.Write(ctx, rw, http.StatusOK, convertGrant(row))
}

// revokeGrant serves POST /v1/grants/{grant_id}/revoke. Revoking a revoked
// grant keeps the first revocation; an unknown grant is not_found. The body
// is empty or {}.
func (s *Server) revokeGrant(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	grantID, err := grantIDParam(r)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if r.ContentLength != 0 {
		var body struct{}
		if err := decodeBody(rw, r, &body); err != nil {
			s.writeErr(ctx, rw, err)
			return
		}
	}
	row, err := s.db.RevokeWorkspaceAdmissionGrant(ctx, database.RevokeWorkspaceAdmissionGrantParams{
		Now: dbtime.Now(),
		ID:  grantID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeError(ctx, rw, http.StatusNotFound, CodeNotFound, "Grant not found.", "")
		return
	}
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("revoke grant: %w", err))
		return
	}
	s.logger.Info(ctx, "workspace admission revoked a grant", slog.F("grant_id", row.ID))
	httpapi.Write(ctx, rw, http.StatusOK, convertGrant(row))
}
