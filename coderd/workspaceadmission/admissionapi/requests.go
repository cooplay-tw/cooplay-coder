package admissionapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/database/provisionerjobs"
	"github.com/coder/coder/v2/coderd/database/pubsub"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/coderd/wspubsub"
	"github.com/coder/coder/v2/provisionersdk"
)

const (
	defaultListLimit = 100
	maxListLimit     = 1000
	maxDisplayBatch  = 1000
	maxReasonLength  = 1024
	// maxGrantTTL bounds how long one grant authorizes acquisition. The
	// controller refreshes longer grants.
	maxGrantTTL = 24 * time.Hour
)

func publishAdmission(ps pubsub.Pubsub, jobID uuid.UUID) error {
	return workspaceadmission.Publish(ps, jobID)
}

func parseStatuses(raw string) ([]database.WorkspaceAdmissionStatus, error) {
	if raw == "" {
		return nil, nil
	}
	var statuses []database.WorkspaceAdmissionStatus
	for _, part := range strings.Split(raw, ",") {
		status := database.WorkspaceAdmissionStatus(strings.TrimSpace(part))
		if !status.Valid() {
			return nil, newAPIError(http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Invalid status %q.", part))
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// listRequests serves GET /v1/requests?status=waiting,granted&limit=N&after=<job_id>.
// Rows are ordered by (queued_at, job_id).
func (s *Server) listRequests(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	statuses, err := parseStatuses(query.Get("status"))
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	limit := defaultListLimit
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxListLimit {
			writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Limit must be between 1 and %d.", maxListLimit), "")
			return
		}
	}
	params := database.GetWorkspaceBuildAdmissionRequestsParams{
		Statuses:   statuses,
		LimitCount: int32(limit), //nolint:gosec // Bounded by maxListLimit.
	}
	if raw := query.Get("after"); raw != "" {
		after, err := uuid.Parse(raw)
		if err != nil {
			writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Invalid after cursor.", "")
			return
		}
		cursor, err := s.db.GetWorkspaceBuildAdmissionByJobID(ctx, after)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "The after cursor does not match a request.", "")
			return
		}
		if err != nil {
			s.writeErr(ctx, rw, xerrors.Errorf("get cursor request: %w", err))
			return
		}
		params.HasCursor = true
		params.AfterQueuedAt = cursor.QueuedAt
		params.AfterJobID = cursor.JobID
	}

	rows, err := s.db.GetWorkspaceBuildAdmissionRequests(ctx, params)
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("list requests: %w", err))
		return
	}
	resp := ListRequestsResponse{Requests: make([]Request, 0, len(rows))}
	for _, row := range rows {
		resp.Requests = append(resp.Requests, convertRequestListRow(row))
	}
	if len(rows) == limit {
		last := rows[len(rows)-1].WorkspaceBuildAdmission.JobID
		resp.NextAfter = &last
	}
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

func (s *Server) showRequest(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobID, err := uuidParam(r, "job_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	req, err := loadRequest(ctx, s.db, jobID)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, req)
}

func loadRequest(ctx context.Context, db database.Store, jobID uuid.UUID) (Request, error) {
	row, err := db.GetWorkspaceBuildAdmissionRequestByJobID(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, newAPIError(http.StatusNotFound, CodeNotFound, "Admission request not found.")
	}
	if err != nil {
		return Request{}, xerrors.Errorf("get request: %w", err)
	}
	return convertRequestRow(row), nil
}

// lockRequest locks the provisioner job row and loads its admission. Every
// admission transition other than acquisition holds this lock, which
// serializes it against provisioners acquiring the job and against build
// cancellation.
func lockRequest(ctx context.Context, tx database.Store, jobID uuid.UUID) (database.ProvisionerJob, database.WorkspaceBuildAdmission, error) {
	job, err := tx.GetProvisionerJobByIDWithLock(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return database.ProvisionerJob{}, database.WorkspaceBuildAdmission{}, newAPIError(http.StatusNotFound, CodeNotFound, "Admission request not found.")
	}
	if err != nil {
		return database.ProvisionerJob{}, database.WorkspaceBuildAdmission{}, xerrors.Errorf("lock provisioner job: %w", err)
	}
	admission, err := tx.GetWorkspaceBuildAdmissionByJobID(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return database.ProvisionerJob{}, database.WorkspaceBuildAdmission{}, newAPIError(http.StatusNotFound, CodeNotFound, "Admission request not found.")
	}
	if err != nil {
		return database.ProvisionerJob{}, database.WorkspaceBuildAdmission{}, xerrors.Errorf("get admission: %w", err)
	}
	return job, admission, nil
}

// conflict builds a 409 that carries the current request state.
func conflict(ctx context.Context, tx database.Store, jobID uuid.UUID, code, message string) error {
	ae := newAPIError(http.StatusConflict, code, message)
	if req, err := loadRequest(ctx, tx, jobID); err == nil {
		ae.resp.Request = &req
	}
	return ae
}

// jobAcquired reports whether a provisioner acquired the job. Rejection and
// the job reaper also set started_at when they fail a pending job, but only
// acquisition assigns a worker.
func jobAcquired(job database.ProvisionerJob, admission database.WorkspaceBuildAdmission) bool {
	return job.WorkerID.Valid || admission.Status == database.WorkspaceAdmissionStatusAcquired
}

func jobPending(job database.ProvisionerJob) bool {
	return !job.StartedAt.Valid && !job.CompletedAt.Valid && !job.CanceledAt.Valid
}

func (s *Server) grantRequest(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobID, err := uuidParam(r, "job_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body GrantRequest
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	switch {
	case body.ReservationID == uuid.Nil:
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field reservation_id is required.", "")
		return
	case body.OperationID == uuid.Nil:
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field operation_id is required.", "")
		return
	case body.Generation < 0:
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field generation must not be negative.", "")
		return
	case body.Profile == "":
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field profile is required.", "")
		return
	case body.GrantTTLSeconds < 1 || time.Duration(body.GrantTTLSeconds)*time.Second > maxGrantTTL:
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Field grant_ttl_seconds must be between 1 and %d.", int64(maxGrantTTL.Seconds())), "")
		return
	}

	var (
		resp Request
		job  database.ProvisionerJob
	)
	err = s.db.InTx(func(tx database.Store) error {
		var admission database.WorkspaceBuildAdmission
		var err error
		job, admission, err = lockRequest(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if admission.Profile != body.Profile {
			return conflict(ctx, tx, jobID, CodeProfileMismatch, fmt.Sprintf("The request asks for profile %q, not %q.", admission.Profile, body.Profile))
		}
		if jobAcquired(job, admission) {
			return conflict(ctx, tx, jobID, CodeAlreadyAcquired, "A provisioner already acquired the job.")
		}
		if !jobPending(job) {
			return conflict(ctx, tx, jobID, CodeJobNotPending, "The job is no longer pending.")
		}
		switch admission.Status {
		case database.WorkspaceAdmissionStatusWaiting:
		case database.WorkspaceAdmissionStatusGranted:
			if admission.ReservationID.UUID != body.ReservationID || admission.OperationID.UUID != body.OperationID {
				return conflict(ctx, tx, jobID, CodeAlreadyGranted, "The request is granted to another reservation or operation.")
			}
			if body.Generation < admission.Generation.Int64 {
				return conflict(ctx, tx, jobID, CodeStaleGeneration, "The grant generation is older than the current grant.")
			}
		default:
			return conflict(ctx, tx, jobID, CodeInvalidStatus, fmt.Sprintf("The request is %s.", admission.Status))
		}

		now := dbtime.Now()
		_, err = tx.GrantWorkspaceBuildAdmission(ctx, database.GrantWorkspaceBuildAdmissionParams{
			ReservationID:  body.ReservationID,
			Generation:     body.Generation,
			OperationID:    body.OperationID,
			GrantExpiresAt: now.Add(time.Duration(body.GrantTTLSeconds) * time.Second),
			Now:            now,
			JobID:          jobID,
		})
		if err != nil {
			return xerrors.Errorf("grant admission: %w", err)
		}
		resp, err = loadRequest(ctx, tx, jobID)
		return err
	}, nil)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}

	// Wake idle provisioners now that the job is eligible.
	if err := provisionerjobs.PostJob(s.pubsub, job); err != nil {
		s.logger.Warn(ctx, "failed to post granted provisioner job", slog.F("job_id", jobID), slog.Error(err))
	}
	s.publish(ctx, jobID)
	s.publishWorkspaceChanged(ctx, resp.OwnerID, resp.WorkspaceID)
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

func (s *Server) revokeRequest(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobID, err := uuidParam(r, "job_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body RevokeRequest
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if len(body.Reason) > maxReasonLength {
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field reason is too long.", "")
		return
	}
	reason := body.Reason
	if reason == "" {
		reason = workspaceadmission.ReasonWaitingForCapacity
	}

	var (
		resp    RevokeResponse
		changed bool
	)
	err = s.db.InTx(func(tx database.Store) error {
		job, admission, err := lockRequest(ctx, tx, jobID)
		if err != nil {
			return err
		}
		switch {
		case jobAcquired(job, admission):
			// The provisioner won the race. Report it so the controller
			// keeps the reservation until the build finishes.
			resp.Acquired = true
		case admission.Status == database.WorkspaceAdmissionStatusGranted:
			if body.ReservationID != uuid.Nil && admission.ReservationID.UUID != body.ReservationID {
				return conflict(ctx, tx, jobID, CodeReservationMismatch, "The request is granted to another reservation.")
			}
			fallthrough
		case admission.Status == database.WorkspaceAdmissionStatusWaiting:
			if jobPending(job) {
				_, err = tx.RequeueWorkspaceBuildAdmission(ctx, database.RequeueWorkspaceBuildAdmissionParams{
					Reason: reason,
					Now:    dbtime.Now(),
					JobID:  jobID,
				})
				if err != nil {
					return xerrors.Errorf("requeue admission: %w", err)
				}
				changed = true
			}
		default:
			// Rejected, canceled and revoked requests can never start, so
			// there is nothing to undo.
		}
		resp.Request, err = loadRequest(ctx, tx, jobID)
		return err
	}, nil)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if changed {
		s.publish(ctx, jobID)
		s.publishWorkspaceChanged(ctx, resp.Request.OwnerID, resp.Request.WorkspaceID)
	}
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

func (s *Server) rejectRequest(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobID, err := uuidParam(r, "job_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body RejectRequest
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if body.Reason == "" {
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field reason is required.", "")
		return
	}
	if len(body.Reason)+len(body.Code) > maxReasonLength {
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Fields code and reason are too long.", "")
		return
	}
	reason := body.Reason
	if body.Code != "" {
		reason = body.Code + ": " + body.Reason
	}
	jobError := "admission rejected: " + reason

	var (
		resp        Request
		changed     bool
		lowestLogID int64
		ownerID     uuid.UUID
		workspaceID uuid.UUID
	)
	err = s.db.InTx(func(tx database.Store) error {
		job, admission, err := lockRequest(ctx, tx, jobID)
		if err != nil {
			return err
		}
		ownerID = admission.OwnerID
		workspaceID = admission.WorkspaceID
		if jobAcquired(job, admission) {
			return conflict(ctx, tx, jobID, CodeAlreadyAcquired, "A provisioner already acquired the job.")
		}
		if admission.Status == database.WorkspaceAdmissionStatusRejected {
			resp, err = loadRequest(ctx, tx, jobID)
			return err
		}
		if !jobPending(job) {
			return conflict(ctx, tx, jobID, CodeJobNotPending, "The job is no longer pending.")
		}
		if admission.Status != database.WorkspaceAdmissionStatusWaiting && admission.Status != database.WorkspaceAdmissionStatusGranted {
			return conflict(ctx, tx, jobID, CodeInvalidStatus, fmt.Sprintf("The request is %s.", admission.Status))
		}

		now := dbtime.Now()
		_, err = tx.UpdatePendingWorkspaceBuildAdmissionStatus(ctx, database.UpdatePendingWorkspaceBuildAdmissionStatusParams{
			Status: database.WorkspaceAdmissionStatusRejected,
			Reason: reason,
			Now:    now,
			JobID:  jobID,
		})
		if err != nil {
			return xerrors.Errorf("reject admission: %w", err)
		}
		logs, err := tx.InsertProvisionerJobLogs(ctx, database.InsertProvisionerJobLogsParams{
			JobID:     jobID,
			CreatedAt: []time.Time{now},
			Source:    []database.LogSource{database.LogSourceProvisionerDaemon},
			Level:     []database.LogLevel{database.LogLevelError},
			Stage:     []string{"Workspace admission"},
			Output:    []string{"Coder: " + jobError},
		})
		if err != nil {
			return xerrors.Errorf("insert rejection log: %w", err)
		}
		if len(logs) > 0 {
			lowestLogID = logs[0].ID
		}
		// The job never started, so record a zero-length run, as the job
		// reaper does for pending jobs.
		err = tx.UpdateProvisionerJobWithCompleteWithStartedAtByID(ctx, database.UpdateProvisionerJobWithCompleteWithStartedAtByIDParams{
			ID:          jobID,
			UpdatedAt:   now,
			CompletedAt: sql.NullTime{Time: now, Valid: true},
			Error:       sql.NullString{String: jobError, Valid: true},
			ErrorCode:   sql.NullString{},
			StartedAt:   sql.NullTime{Time: now, Valid: true},
		})
		if err != nil {
			return xerrors.Errorf("fail rejected job: %w", err)
		}
		changed = true
		resp, err = loadRequest(ctx, tx, jobID)
		return err
	}, nil)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if changed {
		s.publishJobEnded(ctx, jobID, lowestLogID, ownerID, workspaceID)
		s.publish(ctx, jobID)
	}
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

// publishJobEnded tells log streams and workspace watchers that a job ended
// without a provisioner.
func (s *Server) publishJobEnded(ctx context.Context, jobID uuid.UUID, lowestLogID int64, ownerID, workspaceID uuid.UUID) {
	data, err := json.Marshal(provisionersdk.ProvisionerJobLogsNotifyMessage{
		CreatedAfter: lowestLogID - 1,
		EndOfLogs:    true,
	})
	if err == nil {
		err = s.pubsub.Publish(provisionersdk.ProvisionerJobLogsNotifyChannel(jobID), data)
	}
	if err != nil {
		s.logger.Warn(ctx, "failed to publish provisioner job log end", slog.F("job_id", jobID), slog.Error(err))
	}
	s.publishWorkspaceChanged(ctx, ownerID, workspaceID)
}

func (s *Server) publishWorkspaceChanged(ctx context.Context, ownerID, workspaceID uuid.UUID) {
	if err := wspubsub.PublishWorkspaceEvent(ctx, s.pubsub, ownerID, wspubsub.WorkspaceEvent{
		Kind:        wspubsub.WorkspaceEventKindStateChange,
		WorkspaceID: workspaceID,
	}); err != nil {
		s.logger.Warn(ctx, "failed to publish workspace state change", slog.F("workspace_id", workspaceID), slog.Error(err))
	}
	if err := wspubsub.PublishWorkspaceBuildOrchestrationWake(ctx, s.pubsub); err != nil {
		s.logger.Warn(ctx, "failed to publish workspace build orchestration wake", slog.Error(err))
	}
}

// updateDisplays serves POST /v1/requests/display. The body is a JSON array
// of DisplayUpdate. Only waiting requests change.
func (s *Server) updateDisplays(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body []DisplayUpdate
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if len(body) > maxDisplayBatch {
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("At most %d updates are allowed per request.", maxDisplayBatch), "")
		return
	}
	params := database.UpdateWorkspaceBuildAdmissionDisplaysParams{
		UpdatedAt:      dbtime.Now(),
		JobIds:         make([]uuid.UUID, 0, len(body)),
		Reasons:        make([]string, 0, len(body)),
		QueuePositions: make([]int32, 0, len(body)),
	}
	seen := make(map[uuid.UUID]struct{}, len(body))
	for _, update := range body {
		if update.JobID == uuid.Nil {
			writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field job_id is required.", "")
			return
		}
		if _, ok := seen[update.JobID]; ok {
			writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Job %s appears more than once.", update.JobID), "")
			return
		}
		seen[update.JobID] = struct{}{}
		if len(update.Reason) > maxReasonLength {
			writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field reason is too long.", "")
			return
		}
		position := int32(-1)
		if update.QueuePosition != nil {
			if *update.QueuePosition < 0 {
				writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field queue_position must not be negative.", "")
				return
			}
			position = *update.QueuePosition
		}
		params.JobIds = append(params.JobIds, update.JobID)
		params.Reasons = append(params.Reasons, update.Reason)
		params.QueuePositions = append(params.QueuePositions, position)
	}

	resp := DisplayResponse{Updated: []uuid.UUID{}, Skipped: []uuid.UUID{}}
	if len(params.JobIds) > 0 {
		updated, err := s.db.UpdateWorkspaceBuildAdmissionDisplays(ctx, params)
		if err != nil {
			s.writeErr(ctx, rw, xerrors.Errorf("update displays: %w", err))
			return
		}
		updatedSet := make(map[uuid.UUID]struct{}, len(updated))
		for _, row := range updated {
			updatedSet[row.JobID] = struct{}{}
		}
		for _, id := range params.JobIds {
			if _, ok := updatedSet[id]; ok {
				resp.Updated = append(resp.Updated, id)
			} else {
				resp.Skipped = append(resp.Skipped, id)
			}
		}
		for _, id := range resp.Updated {
			s.publish(ctx, id)
		}
		// Workspace pages show the reason and capacity queue position, so
		// their watchers refresh after a change.
		for _, row := range updated {
			s.publishWorkspaceChanged(ctx, row.OwnerID, row.WorkspaceID)
		}
	}
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

func (s *Server) watermark(ctx context.Context) (string, error) {
	latest, err := s.db.GetWorkspaceBuildAdmissionsWatermark(ctx)
	if err != nil {
		return "", xerrors.Errorf("get admission watermark: %w", err)
	}
	return strconv.FormatInt(latest.UnixMicro(), 10), nil
}

// watch serves GET /v1/watch?cursor=...&timeout_seconds=N. It returns as
// soon as the cursor is stale, an admission event or new job posting
// arrives, or the timeout passes. Notifications only shorten the wait;
// controllers must still reconcile with GET /v1/requests.
func (s *Server) watch(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()
	cursor := query.Get("cursor")
	timeout := MaxWatchTimeout
	if raw := query.Get("timeout_seconds"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || time.Duration(seconds)*time.Second > MaxWatchTimeout {
			writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Field timeout_seconds must be between 1 and %d.", int(MaxWatchTimeout.Seconds())), "")
			return
		}
		timeout = time.Duration(seconds) * time.Second
	}

	// Subscribe before reading the watermark so no change slips between
	// the two.
	notify := make(chan struct{}, 1)
	listener := func(context.Context, []byte) {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	cancelAdmission, err := s.pubsub.Subscribe(workspaceadmission.EventChannel, listener)
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("subscribe to admission events: %w", err))
		return
	}
	defer cancelAdmission()
	// New jobs are posted after their build transaction commits, which
	// includes every new waiting admission.
	cancelJobs, err := s.pubsub.Subscribe(provisionerjobs.EventJobPosted, listener)
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("subscribe to job postings: %w", err))
		return
	}
	defer cancelJobs()

	current, err := s.watermark(ctx)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if cursor == "" || cursor != current {
		httpapi.Write(ctx, rw, http.StatusOK, WatchResponse{Cursor: current, Changed: true})
		return
	}

	timer := s.clock.NewTimer(timeout, "admissionapi", "watch")
	defer timer.Stop()
	notified := false
	select {
	case <-ctx.Done():
		return
	case <-notify:
		notified = true
	case <-timer.C:
	}

	current, err = s.watermark(ctx)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, WatchResponse{Cursor: current, Changed: notified || current != cursor})
}
