package admissionapi

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/httpapi"
)

func (s *Server) listUsers(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.GetWorkspaceAdmissionUsers(ctx, uuid.Nil)
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("list users: %w", err))
		return
	}
	resp := ListUsersResponse{Users: make([]User, 0, len(rows))}
	for _, row := range rows {
		resp.Users = append(resp.Users, convertUser(row))
	}
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

func (s *Server) showUser(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, err := uuidParam(r, "user_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	user, err := s.loadUser(r, userID)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, user)
}

func (s *Server) loadUser(r *http.Request, userID uuid.UUID) (User, error) {
	rows, err := s.db.GetWorkspaceAdmissionUsers(r.Context(), userID)
	if err != nil {
		return User{}, xerrors.Errorf("get user: %w", err)
	}
	if len(rows) == 0 {
		return User{}, newAPIError(http.StatusNotFound, CodeNotFound, "User not found.")
	}
	return convertUser(rows[0]), nil
}

// suspendUser suspends a user and deletes all of their API keys, which ends
// every existing session and token immediately.
func (s *Server) suspendUser(rw http.ResponseWriter, r *http.Request) {
	s.setUserStatus(rw, r, database.UserStatusSuspended)
}

func (s *Server) activateUser(rw http.ResponseWriter, r *http.Request) {
	s.setUserStatus(rw, r, database.UserStatusActive)
}

func (s *Server) setUserStatus(rw http.ResponseWriter, r *http.Request, status database.UserStatus) {
	ctx := r.Context()
	userID, err := uuidParam(r, "user_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body UserStatusRequest
	if r.ContentLength != 0 {
		if err := decodeBody(rw, r, &body); err != nil {
			s.writeErr(ctx, rw, err)
			return
		}
	}

	err = s.db.InTx(func(tx database.Store) error {
		user, err := tx.GetUserByID(ctx, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return newAPIError(http.StatusNotFound, CodeNotFound, "User not found.")
		}
		if err != nil {
			return xerrors.Errorf("get user: %w", err)
		}
		if user.IsSystem {
			return newAPIError(http.StatusConflict, CodeSystemUser, "System users cannot change status.")
		}
		if user.Deleted {
			return newAPIError(http.StatusNotFound, CodeNotFound, "User not found.")
		}
		_, err = tx.UpdateUserStatus(ctx, database.UpdateUserStatusParams{
			ID:         userID,
			Status:     status,
			UpdatedAt:  dbtime.Now(),
			UserIsSeen: false,
		})
		if err != nil {
			return xerrors.Errorf("update user status: %w", err)
		}
		if status == database.UserStatusSuspended {
			//nolint:gocritic // Revoking every key of the suspended user is the purpose of this endpoint; the subject can only delete this user's keys.
			if err := tx.DeleteAPIKeysByUserID(dbauthz.AsAPIKeyRevoker(ctx, userID), userID); err != nil {
				return xerrors.Errorf("delete api keys: %w", err)
			}
		}
		return nil
	}, nil)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	s.logger.Info(ctx, "workspace admission changed user status",
		slog.F("user_id", userID),
		slog.F("status", status),
		slog.F("reason", body.Reason),
	)

	user, err := s.loadUser(r, userID)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, user)
}
