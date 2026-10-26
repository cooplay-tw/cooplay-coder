package admissionapi

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/httpapi"
)

func (s *Server) listTemplates(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.GetWorkspaceAdmissionTemplates(ctx)
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("list templates: %w", err))
		return
	}
	resp := ListTemplatesResponse{Templates: make([]Template, 0, len(rows))}
	for _, row := range rows {
		resp.Templates = append(resp.Templates, convertTemplate(row))
	}
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

func (s *Server) showTemplate(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateID, err := uuidParam(r, "template_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	row, err := s.db.GetWorkspaceAdmissionTemplateByTemplateID(ctx, templateID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(ctx, rw, http.StatusNotFound, CodeNotFound, "Template is not managed by workspace admission.", "")
		return
	}
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("get template: %w", err))
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, convertTemplate(row))
}

// putTemplate marks a template as managed. From then on every START build of
// the template needs a grant before a provisioner may acquire it.
func (s *Server) putTemplate(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateID, err := uuidParam(r, "template_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body PutTemplateRequest
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if body.ProfileParameter == "" {
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field profile_parameter is required.", "")
		return
	}
	row, err := s.db.UpsertWorkspaceAdmissionTemplate(ctx, database.UpsertWorkspaceAdmissionTemplateParams{
		ProfileParameter: body.ProfileParameter,
		Now:              dbtime.Now(),
		TemplateID:       templateID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeError(ctx, rw, http.StatusNotFound, CodeNotFound, "Template not found.", "")
		return
	}
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("upsert template: %w", err))
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, convertTemplate(row))
}

// deleteTemplate stops managing a template. Its entitlements are removed with
// it. Existing admission records stay for reconciliation.
func (s *Server) deleteTemplate(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateID, err := uuidParam(r, "template_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	deleted, err := s.db.DeleteWorkspaceAdmissionTemplateByTemplateID(ctx, templateID)
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("delete template: %w", err))
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, DeleteTemplateResponse{Deleted: deleted > 0})
}

func (s *Server) showEntitlements(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, err := uuidParam(r, "user_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if _, err := s.db.GetUserByID(ctx, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(ctx, rw, http.StatusNotFound, CodeNotFound, "User not found.", "")
			return
		}
		s.writeErr(ctx, rw, xerrors.Errorf("get user: %w", err))
		return
	}
	rows, err := s.db.GetWorkspaceAdmissionEntitlementsByUserID(ctx, userID)
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("get entitlements: %w", err))
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, convertEntitlements(userID, rows))
}

// putEntitlements replaces every entitlement of a user in one transaction.
// Templates missing from the body lose their entitlement.
func (s *Server) putEntitlements(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, err := uuidParam(r, "user_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body PutEntitlementsRequest
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	seen := make(map[uuid.UUID]struct{}, len(body.Templates))
	for _, entitlement := range body.Templates {
		if _, ok := seen[entitlement.TemplateID]; ok {
			writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Template %s appears more than once.", entitlement.TemplateID), "")
			return
		}
		seen[entitlement.TemplateID] = struct{}{}
		for _, profile := range entitlement.AllowedProfiles {
			if profile == "" {
				writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Allowed profiles must not be empty strings.", "")
				return
			}
		}
	}

	var rows []database.WorkspaceAdmissionEntitlement
	err = s.db.InTx(func(tx database.Store) error {
		if _, err := tx.GetUserByID(ctx, userID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return newAPIError(http.StatusNotFound, CodeNotFound, "User not found.")
			}
			return xerrors.Errorf("get user: %w", err)
		}
		if err := tx.DeleteWorkspaceAdmissionEntitlementsByUserID(ctx, userID); err != nil {
			return xerrors.Errorf("delete entitlements: %w", err)
		}
		now := dbtime.Now()
		for _, entitlement := range body.Templates {
			profiles := entitlement.AllowedProfiles
			if profiles == nil {
				profiles = []string{}
			}
			row, err := tx.InsertWorkspaceAdmissionEntitlement(ctx, database.InsertWorkspaceAdmissionEntitlementParams{
				UserID:          userID,
				AllowedProfiles: profiles,
				UpdatedAt:       now,
				TemplateID:      entitlement.TemplateID,
			})
			if errors.Is(err, sql.ErrNoRows) {
				return newAPIError(http.StatusBadRequest, CodeTemplateNotManaged, fmt.Sprintf("Template %s is not managed by workspace admission.", entitlement.TemplateID))
			}
			if err != nil {
				return xerrors.Errorf("insert entitlement: %w", err)
			}
			rows = append(rows, row)
		}
		return nil
	}, nil)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, convertEntitlements(userID, rows))
}
