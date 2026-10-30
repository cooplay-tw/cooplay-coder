package admissionapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/coderd/wspubsub"
	"github.com/coder/coder/v2/codersdk"
)

const (
	// maxStateRuns bounds the Runs of a workspace state, as the database
	// CHECK does.
	maxStateRuns = 20
	// maxStateSampleBytes bounds the last sample of a workspace state.
	maxStateSampleBytes = 4096
)

var runStarters = []codersdk.WorkspaceCooplayRunStarter{
	codersdk.WorkspaceCooplayRunStartedByEmployee,
	codersdk.WorkspaceCooplayRunStartedByGrant,
}

// validateState checks a pushed state against the enums and bounds of the
// workspace state table, so invalid pushes fail with invalid_request rather
// than a constraint error.
func validateState(st WorkspaceCooplayState) error {
	invalid := func(message string) error {
		return newAPIError(http.StatusBadRequest, CodeInvalidRequest, message)
	}
	switch {
	case !slices.Contains(codersdk.WorkspaceCooplayPowerStates, st.PowerState):
		return invalid(fmt.Sprintf("Field power_state must be one of %v.", codersdk.WorkspaceCooplayPowerStates))
	case st.PowerEpoch < 0 || st.Generation < 0:
		return invalid("Fields power_epoch and generation must not be negative.")
	case st.Since.IsZero():
		return invalid("Field since is required.")
	case len(st.Reason) > maxReasonLength:
		return invalid("Field reason is too long.")
	case len(st.Runs) > maxStateRuns:
		return invalid(fmt.Sprintf("Field runs has more than %d entries.", maxStateRuns))
	case len(st.LastSample) > maxStateSampleBytes:
		return invalid(fmt.Sprintf("Field last_sample exceeds %d bytes.", maxStateSampleBytes))
	}
	for _, run := range st.Runs {
		if !slices.Contains(runStarters, run.StartedBy) {
			return invalid(fmt.Sprintf("Field runs.started_by must be one of %v.", runStarters))
		}
		if !slices.Contains(codersdk.WorkspaceCooplayRunStates, run.State) {
			return invalid(fmt.Sprintf("Field runs.state must be one of %v.", codersdk.WorkspaceCooplayRunStates))
		}
	}
	return nil
}

// nullJSON encodes an optional fact. A nil value stores SQL NULL.
func nullJSON[T any](v *T) (pqtype.NullRawMessage, error) {
	if v == nil {
		return pqtype.NullRawMessage{}, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return pqtype.NullRawMessage{}, err
	}
	return pqtype.NullRawMessage{RawMessage: raw, Valid: true}, nil
}

func stateParams(workspace database.Workspace, st WorkspaceCooplayState) (database.UpsertWorkspaceAdmissionWorkspaceStateParams, error) {
	runs := st.Runs
	if runs == nil {
		runs = []codersdk.WorkspaceCooplayRun{}
	}
	rawRuns, err := json.Marshal(runs)
	if err != nil {
		return database.UpsertWorkspaceAdmissionWorkspaceStateParams{}, xerrors.Errorf("encode runs: %w", err)
	}
	lastJob, err := nullJSON(st.LastJob)
	if err != nil {
		return database.UpsertWorkspaceAdmissionWorkspaceStateParams{}, xerrors.Errorf("encode last job: %w", err)
	}
	git, err := nullJSON(st.Git)
	if err != nil {
		return database.UpsertWorkspaceAdmissionWorkspaceStateParams{}, xerrors.Errorf("encode git facts: %w", err)
	}
	pr, err := nullJSON(st.PR)
	if err != nil {
		return database.UpsertWorkspaceAdmissionWorkspaceStateParams{}, xerrors.Errorf("encode pull request facts: %w", err)
	}
	var sample pqtype.NullRawMessage
	if len(st.LastSample) > 0 && !bytes.Equal(st.LastSample, []byte("null")) {
		sample = pqtype.NullRawMessage{RawMessage: st.LastSample, Valid: true}
	}
	return database.UpsertWorkspaceAdmissionWorkspaceStateParams{
		WorkspaceID:    workspace.ID,
		OrganizationID: workspace.OrganizationID,
		PowerState:     string(st.PowerState),
		PowerEpoch:     st.PowerEpoch,
		Generation:     st.Generation,
		Since:          dbtime.Time(st.Since),
		Reason:         st.Reason,
		SleepNoticeAt:  nullTime(st.SleepNoticeAt),
		KeepaliveUntil: nullTime(st.KeepaliveUntil),
		Runs:           rawRuns,
		LastJob:        lastJob,
		Git:            git,
		Pr:             pr,
		LastSample:     sample,
		UpdatedAt:      dbtime.Now(),
	}, nil
}

func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: dbtime.Time(*t), Valid: true}
}

func convertWorkspaceState(row database.WorkspaceAdmissionWorkspaceState) (WorkspaceCooplayState, error) {
	st := WorkspaceCooplayState{
		PowerState:     codersdk.WorkspaceCooplayPowerState(row.PowerState),
		PowerEpoch:     row.PowerEpoch,
		Generation:     row.Generation,
		Since:          row.Since,
		Reason:         row.Reason,
		SleepNoticeAt:  timePtr(row.SleepNoticeAt),
		KeepaliveUntil: timePtr(row.KeepaliveUntil),
	}
	if err := json.Unmarshal(row.Runs, &st.Runs); err != nil {
		return WorkspaceCooplayState{}, xerrors.Errorf("decode runs: %w", err)
	}
	for _, fact := range []struct {
		name string
		raw  pqtype.NullRawMessage
		dst  any
	}{
		{"last job", row.LastJob, &st.LastJob},
		{"git facts", row.Git, &st.Git},
		{"pull request facts", row.Pr, &st.PR},
	} {
		if !fact.raw.Valid {
			continue
		}
		if err := json.Unmarshal(fact.raw.RawMessage, fact.dst); err != nil {
			return WorkspaceCooplayState{}, xerrors.Errorf("decode %s: %w", fact.name, err)
		}
	}
	if row.LastSample.Valid {
		st.LastSample = row.LastSample.RawMessage
	}
	return st, nil
}

// managedWorkspace loads a workspace of a managed template, deleted or not.
func managedWorkspace(ctx context.Context, db database.Store, workspaceID uuid.UUID) (database.Workspace, error) {
	workspace, err := db.GetWorkspaceByID(ctx, workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return database.Workspace{}, newAPIError(http.StatusNotFound, CodeNotFound, "Workspace not found.")
	}
	if err != nil {
		return database.Workspace{}, xerrors.Errorf("get workspace: %w", err)
	}
	if _, err := db.GetWorkspaceAdmissionTemplateByTemplateID(ctx, workspace.TemplateID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return database.Workspace{}, newAPIError(http.StatusNotFound, CodeNotFound, "Workspace of a managed template not found.")
		}
		return database.Workspace{}, xerrors.Errorf("get managed template: %w", err)
	}
	return workspace, nil
}

// putWorkspaceState serves PUT /v1/workspaces/{id}/state. The controller
// pushes the power state of a managed workspace with the Runs and facts the
// workspace page shows. The state is stored compare-and-set on power_epoch:
// an older epoch than the stored one fails with 409 stale_power_epoch, also
// after the controller reported the workspace as off, and an equal or newer
// epoch replaces the whole state. It answers 200 with the stored state.
func (s *Server) putWorkspaceState(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := uuidParam(r, "workspace_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body WorkspaceCooplayState
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if err := validateState(body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	workspace, err := managedWorkspace(ctx, s.db, workspaceID)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if workspace.Deleted {
		writeError(ctx, rw, http.StatusConflict, CodeWorkspaceDeleted, "Workspace is deleted.", "")
		return
	}
	params, err := stateParams(workspace, body)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	row, err := s.db.UpsertWorkspaceAdmissionWorkspaceState(ctx, params)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.writeStaleState(ctx, rw, workspaceID, body.PowerEpoch)
		return
	case database.IsCheckViolation(err, database.CheckWorkspaceAdmissionWorkspaceStatesLastSampleCheck):
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Field last_sample exceeds %d bytes as stored.", maxStateSampleBytes), "")
		return
	case err != nil:
		s.writeErr(ctx, rw, xerrors.Errorf("store workspace state: %w", err))
		return
	}
	resp, err := convertWorkspaceState(row)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	s.publishWorkspaceState(ctx, workspace)
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

// writeStaleState answers a rejected state push with the epoch Coder
// stores.
func (s *Server) writeStaleState(ctx context.Context, rw http.ResponseWriter, workspaceID uuid.UUID, epoch int64) {
	stored, err := s.db.GetWorkspaceAdmissionWorkspaceStateByWorkspaceID(ctx, workspaceID)
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("get stored workspace state: %w", err))
		return
	}
	writeError(ctx, rw, http.StatusConflict, CodeStalePowerEpoch,
		fmt.Sprintf("The stored power_epoch %d is newer than %d.", stored.PowerEpoch, epoch), "")
}

// deleteWorkspaceState serves DELETE /v1/workspaces/{id}/state: the
// controller reports the workspace as off. Coder stops showing a power
// state but keeps the power epoch, so a delayed older push is still
// rejected. It answers 204, also when there was no state.
func (s *Server) deleteWorkspaceState(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := uuidParam(r, "workspace_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	workspace, err := managedWorkspace(ctx, s.db, workspaceID)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	cleared, err := s.db.ClearWorkspaceAdmissionWorkspaceState(ctx, database.ClearWorkspaceAdmissionWorkspaceStateParams{
		WorkspaceID: workspaceID,
		Now:         dbtime.Time(s.clock.Now()),
	})
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("clear workspace state: %w", err))
		return
	}
	if cleared > 0 {
		s.publishWorkspaceState(ctx, workspace)
	}
	rw.WriteHeader(http.StatusNoContent)
}

// publishWorkspaceState refreshes workspace watchers, such as the workspace
// page, and admission watchers after a committed state change. Failures are
// only logged because both also poll.
func (s *Server) publishWorkspaceState(ctx context.Context, workspace database.Workspace) {
	if err := wspubsub.PublishWorkspaceEvent(ctx, s.pubsub, workspace.OwnerID, wspubsub.WorkspaceEvent{
		Kind:        wspubsub.WorkspaceEventKindStateChange,
		WorkspaceID: workspace.ID,
	}); err != nil {
		s.logger.Warn(ctx, "failed to publish workspace state change", slog.F("workspace_id", workspace.ID), slog.Error(err))
	}
	if err := workspaceadmission.PublishIntent(s.pubsub, workspaceadmission.EventKindWorkspaceState, workspace.ID); err != nil {
		s.logger.Warn(ctx, "failed to publish workspace admission state event", slog.F("workspace_id", workspace.ID), slog.Error(err))
	}
}
