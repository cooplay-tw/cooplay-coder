package admissionapi

import (
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/httpapi"
)

// Results the controller reports when it acknowledges an intent.
var (
	wakeIntentResults      = []string{WakeResultWoken, WakeResultAlreadyRunning, WakeResultFailed, WakeResultIgnored}
	keepaliveIntentResults = []string{KeepaliveResultApplied, KeepaliveResultDenied}
)

// parseIntentQuery parses ?pending=true|false&limit=N of the intent lists.
// Without pending every intent is listed.
func parseIntentQuery(r *http.Request) (pendingOnly bool, limit int32, err error) {
	query := r.URL.Query()
	if raw := query.Get("pending"); raw != "" {
		pendingOnly, err = strconv.ParseBool(raw)
		if err != nil {
			return false, 0, newAPIError(http.StatusBadRequest, CodeInvalidRequest, "Field pending must be true or false.")
		}
	}
	n := defaultListLimit
	if raw := query.Get("limit"); raw != "" {
		n, err = strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxListLimit {
			return false, 0, newAPIError(http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Limit must be between 1 and %d.", maxListLimit))
		}
	}
	return pendingOnly, int32(n), nil //nolint:gosec // Bounded by maxListLimit.
}

// listWakeIntents serves GET /v1/wake-intents?pending=true&limit=N, oldest
// request first.
func (s *Server) listWakeIntents(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pendingOnly, limit, err := parseIntentQuery(r)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	rows, err := s.db.GetWorkspaceAdmissionWakeIntents(ctx, database.GetWorkspaceAdmissionWakeIntentsParams{
		PendingOnly: pendingOnly,
		LimitCount:  limit,
	})
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("list wake intents: %w", err))
		return
	}
	resp := ListWakeIntentsResponse{Intents: make([]WakeIntent, 0, len(rows))}
	for _, row := range rows {
		resp.Intents = append(resp.Intents, convertWakeIntent(row))
	}
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

// ackWakeIntent serves POST /v1/wake-intents/{workspace_id}/ack. The
// acknowledgement only applies when last_requested_at is still the latest
// request, so a request recorded while the controller was waking the
// workspace stays pending. It answers acked=false otherwise, also when the
// workspace has no intent.
func (s *Server) ackWakeIntent(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := uuidParam(r, "workspace_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body AckWakeIntentRequest
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	switch {
	case body.LastRequestedAt.IsZero():
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field last_requested_at is required.", "")
		return
	case body.PowerEpoch < 0:
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field power_epoch must not be negative.", "")
		return
	case !slices.Contains(wakeIntentResults, body.Result):
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Field result must be one of %v.", wakeIntentResults), "")
		return
	}
	rows, err := s.db.AckWorkspaceAdmissionWakeIntent(ctx, database.AckWorkspaceAdmissionWakeIntentParams{
		Now:             dbtime.Time(s.clock.Now()),
		PowerEpoch:      body.PowerEpoch,
		Result:          body.Result,
		WorkspaceID:     workspaceID,
		LastRequestedAt: body.LastRequestedAt,
	})
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("acknowledge wake intent: %w", err))
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, AckIntentResponse{Acked: rows > 0})
}

// listKeepaliveIntents serves GET /v1/keepalive-intents?pending=true&limit=N,
// oldest request first.
func (s *Server) listKeepaliveIntents(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pendingOnly, limit, err := parseIntentQuery(r)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	rows, err := s.db.GetWorkspaceAdmissionKeepaliveIntents(ctx, database.GetWorkspaceAdmissionKeepaliveIntentsParams{
		PendingOnly: pendingOnly,
		LimitCount:  limit,
	})
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("list keepalive intents: %w", err))
		return
	}
	resp := ListKeepaliveIntentsResponse{Intents: make([]KeepaliveIntent, 0, len(rows))}
	for _, row := range rows {
		resp.Intents = append(resp.Intents, convertKeepaliveIntent(row))
	}
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

// ackKeepaliveIntent serves POST /v1/keepalive-intents/{workspace_id}/ack.
// The acknowledgement only applies when requested_at is still the latest
// request, so a newer press of the keepalive button stays pending. It
// answers acked=false otherwise, also when the workspace has no intent. An
// applied keepalive must name its end; a denial records none.
func (s *Server) ackKeepaliveIntent(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := uuidParam(r, "workspace_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body AckKeepaliveIntentRequest
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	hasUntil := body.Until != nil && !body.Until.IsZero()
	switch {
	case body.RequestedAt.IsZero():
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field requested_at is required.", "")
		return
	case !slices.Contains(keepaliveIntentResults, body.Result):
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Field result must be one of %v.", keepaliveIntentResults), "")
		return
	case body.Result == KeepaliveResultApplied && !hasUntil:
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field until is required when result is applied.", "")
		return
	}
	until := sql.NullTime{}
	if body.Result == KeepaliveResultApplied {
		until = sql.NullTime{Time: *body.Until, Valid: true}
	}
	rows, err := s.db.AckWorkspaceAdmissionKeepaliveIntent(ctx, database.AckWorkspaceAdmissionKeepaliveIntentParams{
		Now:         dbtime.Time(s.clock.Now()),
		Until:       until,
		Result:      body.Result,
		WorkspaceID: workspaceID,
		RequestedAt: body.RequestedAt,
	})
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("acknowledge keepalive intent: %w", err))
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, AckIntentResponse{Acked: rows > 0})
}
