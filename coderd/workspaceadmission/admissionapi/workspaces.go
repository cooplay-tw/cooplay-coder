package admissionapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/audit"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/database/provisionerjobs"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/coderd/wsbuilder"
)

func (s *Server) listWorkspaces(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.GetWorkspaceAdmissionWorkspaces(ctx, uuid.Nil)
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("list workspaces: %w", err))
		return
	}
	resp := ListWorkspacesResponse{Workspaces: make([]Workspace, 0, len(rows))}
	for _, row := range rows {
		resp.Workspaces = append(resp.Workspaces, convertWorkspace(row))
	}
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}

func (s *Server) showWorkspace(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := uuidParam(r, "workspace_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	rows, err := s.db.GetWorkspaceAdmissionWorkspaces(ctx, workspaceID)
	if err != nil {
		s.writeErr(ctx, rw, xerrors.Errorf("get workspace: %w", err))
		return
	}
	if len(rows) == 0 {
		writeError(ctx, rw, http.StatusNotFound, CodeNotFound, "Workspace of a managed template not found.", "")
		return
	}
	httpapi.Write(ctx, rw, http.StatusOK, convertWorkspace(rows[0]))
}

// loadBuildJob loads a workspace build, its job and its admission, if any.
func loadBuildJob(ctx context.Context, tx database.Store, buildID uuid.UUID) (BuildJob, error) {
	build, err := tx.GetWorkspaceBuildByID(ctx, buildID)
	if err != nil {
		return BuildJob{}, xerrors.Errorf("get workspace build: %w", err)
	}
	job, err := tx.GetProvisionerJobByID(ctx, build.JobID)
	if err != nil {
		return BuildJob{}, xerrors.Errorf("get provisioner job: %w", err)
	}
	admission, err := tx.GetWorkspaceBuildAdmissionByJobID(ctx, build.JobID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BuildJob{}, xerrors.Errorf("get admission: %w", err)
	}
	if err != nil {
		return convertBuildJob(build, job, nil), nil
	}
	return convertBuildJob(build, job, &admission), nil
}

// stopWorkspace serves POST /v1/workspaces/{id}/stop. It creates a STOP build
// on behalf of the platform, the way the lifecycle executor creates system
// builds. A pending START that no provisioner acquired is canceled in the
// same transaction. A build that a provisioner already runs makes the request
// fail with active_build, so the controller can use running cancellation.
// Requests are idempotent per operation_id.
func (s *Server) stopWorkspace(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := uuidParam(r, "workspace_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body StopWorkspaceRequest
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if body.OperationID == uuid.Nil {
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field operation_id is required.", "")
		return
	}
	if len(body.Reason) > maxReasonLength {
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field reason is too long.", "")
		return
	}

	var (
		resp     StopWorkspaceResponse
		stopJob  *database.ProvisionerJob
		canceled bool
		ownerID  uuid.UUID
	)
	err = s.db.InTx(func(tx database.Store) error {
		resp = StopWorkspaceResponse{}
		stopJob = nil
		canceled = false

		existing, done, err := s.existingStopOperation(ctx, tx, body.OperationID, workspaceID)
		if err != nil || done {
			resp = existing
			return err
		}

		workspace, err := tx.GetWorkspaceByID(ctx, workspaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return newAPIError(http.StatusNotFound, CodeNotFound, "Workspace not found.")
		}
		if err != nil {
			return xerrors.Errorf("get workspace: %w", err)
		}
		ownerID = workspace.OwnerID
		if _, err := tx.GetWorkspaceAdmissionTemplateByTemplateID(ctx, workspace.TemplateID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return newAPIError(http.StatusNotFound, CodeNotFound, "Workspace of a managed template not found.")
			}
			return xerrors.Errorf("get managed template: %w", err)
		}
		if workspace.Deleted {
			return newAPIError(http.StatusConflict, CodeWorkspaceDeleted, "Workspace is deleted.")
		}

		latest, err := tx.GetLatestWorkspaceBuildByWorkspaceID(ctx, workspaceID)
		if err != nil {
			return xerrors.Errorf("get latest build: %w", err)
		}
		latestJob, err := tx.GetProvisionerJobByIDWithLock(ctx, latest.JobID)
		if err != nil {
			return xerrors.Errorf("lock latest job: %w", err)
		}
		var canceledJobID *uuid.UUID
		if !latestJob.CompletedAt.Valid {
			if latest.Transition != database.WorkspaceTransitionStart || latestJob.WorkerID.Valid || latestJob.StartedAt.Valid || latestJob.CanceledAt.Valid {
				ae := newAPIError(http.StatusConflict, CodeActiveBuild, "Workspace has an active build that a provisioner owns or that is not a pending START.")
				if bj, err := loadBuildJob(ctx, tx, latest.ID); err == nil {
					ae.resp.Job = &bj
				}
				return ae
			}
			// No provisioner acquired the START and we hold its job row
			// lock, so cancel it before it can start.
			now := dbtime.Now()
			rows, err := tx.UpdateManagedWorkspaceBuildJobWithCancelByID(ctx, database.UpdateManagedWorkspaceBuildJobWithCancelByIDParams{
				ID:          latestJob.ID,
				CanceledAt:  sql.NullTime{Time: now, Valid: true},
				CompletedAt: sql.NullTime{Time: now, Valid: true},
			})
			if err != nil {
				return xerrors.Errorf("cancel pending start: %w", err)
			}
			if rows != 1 {
				return xerrors.Errorf("cancel pending start: updated %d jobs", rows)
			}
			_, err = tx.UpdatePendingWorkspaceBuildAdmissionStatus(ctx, database.UpdatePendingWorkspaceBuildAdmissionStatusParams{
				Status: database.WorkspaceAdmissionStatusCanceled,
				Reason: workspaceadmission.ReasonStoppedByController,
				Now:    now,
				JobID:  latestJob.ID,
			})
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return xerrors.Errorf("cancel pending start admission: %w", err)
			}
			canceled = true
			id := latestJob.ID
			canceledJobID = &id
		}

		// The platform stops the workspace without a schedule, like the
		// lifecycle executor's failed-build cleanup, so the build carries the
		// autostop reason and the workspace owner as initiator.
		builder := wsbuilder.New(workspace, database.WorkspaceTransitionStop, *s.buildUsageChecker.Load()).
			Reason(database.BuildReasonAutostop).
			Experiments(s.experiments).
			DeploymentValues(s.deploymentValues).
			BuildMetrics(s.buildMetrics)
		//nolint:gocritic // System STOP builds run as the lifecycle executor subject, as autostop does.
		build, job, _, err := builder.Build(dbauthz.AsAutostart(ctx), tx, s.fileCache, nil, audit.WorkspaceBuildBaggage{})
		if err != nil {
			var buildErr wsbuilder.BuildError
			if errors.As(err, &buildErr) && buildErr.Status < http.StatusInternalServerError {
				code := CodeInvalidRequest
				if buildErr.Status == http.StatusConflict {
					code = CodeActiveBuild
				}
				ae := newAPIError(buildErr.Status, code, buildErr.Message)
				ae.resp.Detail = buildErr.Error()
				return ae
			}
			return xerrors.Errorf("build stop: %w", err)
		}
		stopJob = job

		_, err = tx.InsertWorkspaceAdmissionStopOperation(ctx, database.InsertWorkspaceAdmissionStopOperationParams{
			OperationID:      body.OperationID,
			WorkspaceID:      workspaceID,
			WorkspaceBuildID: build.ID,
			JobID:            job.ID,
			OrganizationID:   workspace.OrganizationID,
			CanceledJobID:    uuid.NullUUID{UUID: latestJob.ID, Valid: canceledJobID != nil},
			Reason:           body.Reason,
			CreatedAt:        dbtime.Now(),
		})
		if err != nil {
			return xerrors.Errorf("insert stop operation: %w", err)
		}
		bj, err := loadBuildJob(ctx, tx, build.ID)
		if err != nil {
			return err
		}
		resp = StopWorkspaceResponse{
			OperationID:   body.OperationID,
			WorkspaceID:   workspaceID,
			Created:       true,
			Build:         bj,
			CanceledJobID: canceledJobID,
		}
		return nil
	}, nil)
	if err != nil && (database.IsUniqueViolation(err, database.UniqueWorkspaceAdmissionStopOperationsPkey) ||
		database.IsUniqueViolation(err, database.UniqueWorkspaceBuildsWorkspaceIDBuildNumberKey)) {
		// A concurrent request with the same operation won the race, or
		// another build was inserted concurrently. Return the original
		// operation if there is one.
		existing, done, lookupErr := s.existingStopOperation(ctx, s.db, body.OperationID, workspaceID)
		switch {
		case lookupErr != nil:
			err = lookupErr
		case done:
			resp = existing
			stopJob = nil
			canceled = false
			err = nil
		default:
			err = newAPIError(http.StatusConflict, CodeActiveBuild, "Another build was created concurrently. Retry the request.")
		}
	}
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}

	if stopJob != nil {
		if err := provisionerjobs.PostJob(s.pubsub, *stopJob); err != nil {
			s.logger.Warn(ctx, "failed to post stop provisioner job", slog.F("job_id", stopJob.ID), slog.Error(err))
		}
		if canceled && resp.CanceledJobID != nil {
			s.publish(ctx, *resp.CanceledJobID)
		}
		s.publishWorkspaceChanged(ctx, ownerID, workspaceID)
	}
	status := http.StatusOK
	if resp.Created {
		status = http.StatusCreated
	}
	httpapi.Write(ctx, rw, status, resp)
}

// existingStopOperation returns the recorded result of a stop operation. It
// reports done when the operation already exists for this workspace.
func (*Server) existingStopOperation(ctx context.Context, db database.Store, operationID, workspaceID uuid.UUID) (StopWorkspaceResponse, bool, error) {
	op, err := db.GetWorkspaceAdmissionStopOperationByOperationID(ctx, operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return StopWorkspaceResponse{}, false, nil
	}
	if err != nil {
		return StopWorkspaceResponse{}, false, xerrors.Errorf("get stop operation: %w", err)
	}
	if op.WorkspaceID != workspaceID {
		return StopWorkspaceResponse{}, false, newAPIError(http.StatusConflict, CodeOperationConflict, "The operation already stopped another workspace.")
	}
	bj, err := loadBuildJob(ctx, db, op.WorkspaceBuildID)
	if err != nil {
		return StopWorkspaceResponse{}, false, err
	}
	resp := StopWorkspaceResponse{
		OperationID: op.OperationID,
		WorkspaceID: op.WorkspaceID,
		Created:     false,
		Build:       bj,
	}
	if op.CanceledJobID.Valid {
		id := op.CanceledJobID.UUID
		resp.CanceledJobID = &id
	}
	return resp, true, nil
}

// cancelJob serves POST /v1/jobs/{job_id}/cancel with the semantics of the
// workspace build cancel endpoint: a job no provisioner acquired is canceled
// immediately and its admission ends; an acquired job is marked canceling
// and the provisioner running it stops it.
func (s *Server) cancelJob(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobID, err := uuidParam(r, "job_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}

	var (
		resp              CancelJobResponse
		admissionCanceled bool
		ownerID           uuid.UUID
		workspaceID       uuid.UUID
	)
	err = s.db.InTx(func(tx database.Store) error {
		job, err := tx.GetProvisionerJobByIDWithLock(ctx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			return newAPIError(http.StatusNotFound, CodeNotFound, "Job not found.")
		}
		if err != nil {
			return xerrors.Errorf("lock job: %w", err)
		}
		if job.Type != database.ProvisionerJobTypeWorkspaceBuild {
			return newAPIError(http.StatusNotFound, CodeNotFound, "Workspace build job of a managed template not found.")
		}
		build, err := tx.GetWorkspaceBuildByJobID(ctx, jobID)
		if err != nil {
			return xerrors.Errorf("get workspace build: %w", err)
		}
		if job.CompletedAt.Valid || job.CanceledAt.Valid {
			code, message := CodeJobCompleted, "The job has already completed."
			if !job.CompletedAt.Valid {
				code, message = CodeJobCanceling, "The job is already being canceled."
			}
			ae := newAPIError(http.StatusConflict, code, message)
			if bj, err := loadBuildJob(ctx, tx, build.ID); err == nil {
				ae.resp.Job = &bj
			}
			return ae
		}

		now := dbtime.Now()
		rows, err := tx.UpdateManagedWorkspaceBuildJobWithCancelByID(ctx, database.UpdateManagedWorkspaceBuildJobWithCancelByIDParams{
			ID:         jobID,
			CanceledAt: sql.NullTime{Time: now, Valid: true},
			// A job with a worker is running; the provisioner completes it.
			CompletedAt: sql.NullTime{Time: now, Valid: !job.WorkerID.Valid},
		})
		if err != nil {
			return xerrors.Errorf("cancel job: %w", err)
		}
		if rows == 0 {
			return newAPIError(http.StatusNotFound, CodeNotFound, "Workspace build job of a managed template not found.")
		}
		if !job.WorkerID.Valid {
			admissionCanceled, err = workspaceadmission.CancelPending(ctx, tx, jobID, now)
			if err != nil {
				return err
			}
		}
		workspace, err := tx.GetWorkspaceByID(ctx, build.WorkspaceID)
		if err != nil {
			return xerrors.Errorf("get workspace: %w", err)
		}
		ownerID = workspace.OwnerID
		workspaceID = workspace.ID
		after, err := tx.GetProvisionerJobByID(ctx, jobID)
		if err != nil {
			return xerrors.Errorf("get job: %w", err)
		}
		resp = CancelJobResponse{
			JobID:               jobID,
			CanceledBeforeStart: !job.WorkerID.Valid,
			Job:                 convertProvisionerJobState(after),
		}
		return nil
	}, nil)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if admissionCanceled {
		s.publish(ctx, jobID)
	}
	s.publishWorkspaceChanged(ctx, ownerID, workspaceID)
	httpapi.Write(ctx, rw, http.StatusOK, resp)
}
