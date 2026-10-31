package admissionapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/audit"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/database/provisionerjobs"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/coderd/httpapi/httperror"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/coderd/wsbuilder"
	"github.com/coder/coder/v2/codersdk"
)

var (
	// controllerIDPattern matches the opaque IDs of controller grants and
	// Runs (for example g-<ulid>), which also admits UUIDs.
	controllerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	// harnessPattern matches controller harness names.
	harnessPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

	createSourceKinds = []string{CreateSourceEmployee, CreateSourceAgentGrant}

	// errOperationExists aborts a create transaction whose operation was
	// committed by a concurrent request.
	errOperationExists = xerrors.New("create operation already exists")
)

const (
	maxProfileLength    = 64
	maxNamePrefixLength = 32
)

// createRequest is a validated CreateWorkspaceRequest.
type createRequest struct {
	body              CreateWorkspaceRequest
	templateVersionID uuid.UUID
	source            json.RawMessage
}

func validateCreateRequest(body CreateWorkspaceRequest) (createRequest, error) {
	invalid := func(msg string) (createRequest, error) {
		return createRequest{}, newAPIError(http.StatusBadRequest, CodeInvalidRequest, msg)
	}
	switch {
	case body.OperationID == uuid.Nil:
		return invalid("Field operation_id is required.")
	case body.OwnerID == uuid.Nil:
		return invalid("Field owner_id is required.")
	case body.TemplateID == uuid.Nil:
		return invalid("Field template_id is required.")
	case body.Profile == "" || len(body.Profile) > maxProfileLength:
		return invalid(fmt.Sprintf("Field profile is required and at most %d bytes long.", maxProfileLength))
	case !slices.Contains(createSourceKinds, body.Source.Kind):
		return invalid(fmt.Sprintf("Field source.kind must be one of %v.", createSourceKinds))
	case body.Source.Kind == CreateSourceAgentGrant && body.Source.GrantID == "":
		return invalid("Field source.grant_id is required for agent_grant creates.")
	}
	if err := codersdk.NameValid(body.Name); err != nil {
		return invalid("Field name is not a valid workspace name: " + err.Error())
	}
	for field, id := range map[string]string{"source.grant_id": body.Source.GrantID, "source.run_id": body.Source.RunID} {
		if id != "" && !controllerIDPattern.MatchString(id) {
			return invalid(fmt.Sprintf("Field %s must match %s.", field, controllerIDPattern))
		}
	}
	if body.Source.Harness != "" && !harnessPattern.MatchString(body.Source.Harness) {
		return invalid(fmt.Sprintf("Field source.harness must match %s.", harnessPattern))
	}
	for field, id := range map[string]string{
		"source.initiator_workspace_id": body.Source.InitiatorWorkspaceID,
		"source.clone_of_workspace_id":  body.Source.CloneOfWorkspaceID,
	} {
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			return invalid(fmt.Sprintf("Field %s must be a UUID.", field))
		}
	}
	for _, p := range body.RichParameterValues {
		if p.Name == "" {
			return invalid("Rich parameter names must not be empty.")
		}
	}
	req := createRequest{body: body}
	if body.TemplateVersionID != "" {
		id, err := uuid.Parse(body.TemplateVersionID)
		if err != nil || id == uuid.Nil {
			return invalid("Field template_version_id must be a UUID.")
		}
		req.templateVersionID = id
	}
	source, err := json.Marshal(body.Source)
	if err != nil {
		return createRequest{}, xerrors.Errorf("marshal create source: %w", err)
	}
	req.source = source
	return req, nil
}

// createWorkspace serves POST /v1/workspaces. Coder creates the workspace
// and its START build on behalf of the owner, with the owner's own
// permissions, in one transaction that also records the waiting admission
// of the build, the create operation and, under a grant, checks the grant
// mirror with the grant row locked. It answers 201 for a new workspace and
// 200 with the original workspace when the operation already exists.
func (s *Server) createWorkspace(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.createWorkspaceFn == nil {
		writeError(ctx, rw, http.StatusNotImplemented, CodeInternal, "Creating workspaces is not configured on this server.", "")
		return
	}
	var body CreateWorkspaceRequest
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	req, err := validateCreateRequest(body)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}

	if resp, done, err := s.existingCreateOperation(ctx, s.db, req); err != nil || done {
		if err != nil {
			s.writeErr(ctx, rw, err)
			return
		}
		httpapi.Write(ctx, rw, http.StatusOK, resp)
		return
	}

	template, params, err := s.checkCreate(ctx, req)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}

	sdkReq := codersdk.CreateWorkspaceRequest{
		Name:                body.Name,
		RichParameterValues: params,
	}
	if req.templateVersionID != uuid.Nil {
		sdkReq.TemplateVersionID = req.templateVersionID
	} else {
		sdkReq.TemplateID = body.TemplateID
	}

	var created database.WorkspaceAdmissionCreateOperation
	_, err = s.createWorkspaceFn(ctx, workspaceadmission.CreateWorkspaceParams{
		OwnerID: body.OwnerID,
		Request: sdkReq,
		// The hooks get the owner's context; admission records are written
		// with the admission service context of this request instead.
		PreCreateInTX: func(_ context.Context, tx database.Store) error {
			if _, err := tx.GetWorkspaceAdmissionCreateOperationByOperationID(ctx, body.OperationID); err == nil {
				return errOperationExists
			} else if !errors.Is(err, sql.ErrNoRows) {
				return xerrors.Errorf("get create operation: %w", err)
			}
			if body.Source.GrantID == "" {
				return nil
			}
			return s.checkCreateGrant(ctx, tx, req, template)
		},
		PostBuildInTX: func(_ context.Context, tx database.Store, workspace database.Workspace, build database.WorkspaceBuild, job database.ProvisionerJob) error {
			op, err := tx.InsertWorkspaceAdmissionCreateOperation(ctx, database.InsertWorkspaceAdmissionCreateOperationParams{
				OperationID:      body.OperationID,
				WorkspaceID:      workspace.ID,
				WorkspaceBuildID: build.ID,
				JobID:            job.ID,
				OrganizationID:   workspace.OrganizationID,
				OwnerID:          workspace.OwnerID,
				GrantID:          sql.NullString{String: body.Source.GrantID, Valid: body.Source.GrantID != ""},
				RunID:            sql.NullString{String: body.Source.RunID, Valid: body.Source.RunID != ""},
				Source:           req.source,
				CreatedAt:        dbtime.Now(),
			})
			if err != nil {
				return xerrors.Errorf("insert create operation: %w", err)
			}
			created = op
			return nil
		},
	})
	if err != nil {
		s.writeCreateError(ctx, rw, req, err)
		return
	}

	admission, err := loadRequest(ctx, s.db, created.JobID)
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	s.logger.Info(ctx, "workspace admission created a workspace on behalf of its owner",
		slog.F("operation_id", created.OperationID),
		slog.F("workspace_id", created.WorkspaceID),
		slog.F("owner_id", created.OwnerID),
		slog.F("grant_id", created.GrantID.String),
		slog.F("run_id", created.RunID.String),
	)
	httpapi.Write(ctx, rw, http.StatusCreated, CreateWorkspaceResponse{
		Created:     true,
		WorkspaceID: created.WorkspaceID,
		BuildID:     created.WorkspaceBuildID,
		JobID:       created.JobID,
		Admission:   admission,
	})
}

// checkCreate fails fast on requests the create would reject anyway: an
// unknown owner, a template that is not managed, a version of another
// template, a profile that the parameters contradict, or a profile the owner
// is not entitled to. It returns the template and the build parameters with
// the profile parameter set.
func (s *Server) checkCreate(ctx context.Context, req createRequest) (database.Template, []codersdk.WorkspaceBuildParameter, error) {
	body := req.body
	if _, err := s.db.GetUserByID(ctx, body.OwnerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return database.Template{}, nil, newAPIError(http.StatusNotFound, CodeNotFound, "Owner not found.")
		}
		return database.Template{}, nil, xerrors.Errorf("get owner: %w", err)
	}
	managed, err := s.db.GetWorkspaceAdmissionTemplateByTemplateID(ctx, body.TemplateID)
	if errors.Is(err, sql.ErrNoRows) {
		return database.Template{}, nil, newAPIError(http.StatusBadRequest, CodeTemplateNotManaged, "The template is not managed by workspace admission.")
	}
	if err != nil {
		return database.Template{}, nil, xerrors.Errorf("get managed template: %w", err)
	}
	template, err := s.db.GetTemplateByID(ctx, body.TemplateID)
	if err != nil {
		return database.Template{}, nil, xerrors.Errorf("get template: %w", err)
	}
	if req.templateVersionID != uuid.Nil {
		version, err := s.db.GetTemplateVersionByID(ctx, req.templateVersionID)
		if errors.Is(err, sql.ErrNoRows) {
			return database.Template{}, nil, newAPIError(http.StatusBadRequest, CodeInvalidRequest, "Template version not found.")
		}
		if err != nil {
			return database.Template{}, nil, xerrors.Errorf("get template version: %w", err)
		}
		if !version.TemplateID.Valid || version.TemplateID.UUID != template.ID {
			return database.Template{}, nil, newAPIError(http.StatusBadRequest, CodeInvalidRequest, "The template version belongs to another template.")
		}
	}

	params := make([]codersdk.WorkspaceBuildParameter, 0, len(body.RichParameterValues)+1)
	profileSet := false
	for _, p := range body.RichParameterValues {
		if p.Name == managed.ProfileParameter {
			if p.Value != body.Profile {
				return database.Template{}, nil, newAPIError(http.StatusBadRequest, CodeProfileMismatch,
					fmt.Sprintf("Parameter %q is %q, but the requested profile is %q.", p.Name, p.Value, body.Profile))
			}
			profileSet = true
		}
		params = append(params, codersdk.WorkspaceBuildParameter{Name: p.Name, Value: p.Value})
	}
	if !profileSet {
		params = append(params, codersdk.WorkspaceBuildParameter{Name: managed.ProfileParameter, Value: body.Profile})
	}

	entitlement, err := s.db.GetWorkspaceAdmissionEntitlementByUserIDAndTemplateID(ctx, database.GetWorkspaceAdmissionEntitlementByUserIDAndTemplateIDParams{
		UserID:     body.OwnerID,
		TemplateID: template.ID,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return database.Template{}, nil, xerrors.Errorf("get entitlement: %w", err)
	}
	if err != nil || !slices.Contains(entitlement.AllowedProfiles, body.Profile) {
		return database.Template{}, nil, newAPIError(http.StatusForbidden, CodeNotEntitled,
			fmt.Sprintf("The owner is not entitled to profile %q on this template.", body.Profile))
	}
	return template, params, nil
}

// checkCreateGrant checks the grant mirror of a create with the grant row
// locked, so creates under the same grant run one after another and each
// counts the workspaces the earlier ones committed. The controller's count
// is authoritative; this is a backstop.
func (s *Server) checkCreateGrant(ctx context.Context, tx database.Store, req createRequest, template database.Template) error {
	body := req.body
	grant, err := s.lockLiveGrant(ctx, tx, body.Source.GrantID, body.OwnerID)
	if err != nil {
		return err
	}
	switch {
	case grant.OrganizationID != template.OrganizationID:
		return newAPIError(http.StatusForbidden, CodeGrantScope, "The grant does not cover the organization of the template.")
	case len(grant.TemplateIDs) > 0 && !slices.Contains(grant.TemplateIDs, template.ID):
		return newAPIError(http.StatusForbidden, CodeGrantScope, "The grant does not cover the template.")
	case !strings.HasPrefix(body.Name, grant.NamePrefix):
		return newAPIError(http.StatusForbidden, CodeGrantScope, fmt.Sprintf("Workspaces created under the grant must have names starting with %q.", grant.NamePrefix))
	}
	live, err := tx.CountLiveWorkspaceAdmissionCreateOperationsByGrantID(ctx, grant.ID)
	if err != nil {
		return xerrors.Errorf("count workspaces of grant: %w", err)
	}
	if live >= grant.MaxWorkspaces {
		return newAPIError(http.StatusForbidden, CodeGrantQuota, fmt.Sprintf("The grant already has %d of at most %d workspaces.", live, grant.MaxWorkspaces))
	}
	return nil
}

// lockLiveGrant locks a mirrored grant until the transaction ends and checks
// that it belongs to ownerID and is neither revoked nor expired.
func (s *Server) lockLiveGrant(ctx context.Context, tx database.Store, grantID string, ownerID uuid.UUID) (database.WorkspaceAdmissionGrant, error) {
	grant, err := tx.GetWorkspaceAdmissionGrantByIDWithLock(ctx, grantID)
	if errors.Is(err, sql.ErrNoRows) {
		return database.WorkspaceAdmissionGrant{}, newAPIError(http.StatusForbidden, CodeGrantNotFound, "The grant is not known.")
	}
	if err != nil {
		return database.WorkspaceAdmissionGrant{}, xerrors.Errorf("lock grant: %w", err)
	}
	switch {
	case grant.OwnerID != ownerID:
		return database.WorkspaceAdmissionGrant{}, newAPIError(http.StatusForbidden, CodeGrantScope, "The grant belongs to another owner.")
	case grant.RevokedAt.Valid:
		return database.WorkspaceAdmissionGrant{}, newAPIError(http.StatusForbidden, CodeGrantRevoked, "The grant was revoked.")
	case !s.clock.Now().Before(grant.ExpiresAt):
		return database.WorkspaceAdmissionGrant{}, newAPIError(http.StatusForbidden, CodeGrantExpired, "The grant expired.")
	}
	return grant, nil
}

// existingCreateOperation returns the workspace of an operation that already
// exists. It reports done when the operation exists for the same owner,
// template and grant, and fails with operation_conflict when it created a
// different workspace.
func (*Server) existingCreateOperation(ctx context.Context, db database.Store, req createRequest) (CreateWorkspaceResponse, bool, error) {
	op, err := db.GetWorkspaceAdmissionCreateOperationByOperationID(ctx, req.body.OperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return CreateWorkspaceResponse{}, false, nil
	}
	if err != nil {
		return CreateWorkspaceResponse{}, false, xerrors.Errorf("get create operation: %w", err)
	}
	workspace, err := db.GetWorkspaceByID(ctx, op.WorkspaceID)
	if err != nil {
		return CreateWorkspaceResponse{}, false, xerrors.Errorf("get workspace of create operation: %w", err)
	}
	if op.OwnerID != req.body.OwnerID || workspace.TemplateID != req.body.TemplateID || op.GrantID.String != req.body.Source.GrantID {
		return CreateWorkspaceResponse{}, false, newAPIError(http.StatusConflict, CodeOperationConflict, "The operation already created another workspace.")
	}
	admission, err := loadRequest(ctx, db, op.JobID)
	if err != nil {
		return CreateWorkspaceResponse{}, false, err
	}
	return CreateWorkspaceResponse{
		Created:     false,
		WorkspaceID: op.WorkspaceID,
		BuildID:     op.WorkspaceBuildID,
		JobID:       op.JobID,
		Admission:   admission,
	}, true, nil
}

// writeCreateError answers a failed create. A create that lost a race
// against a concurrent request with the same operation answers with the
// original workspace, whatever made it fail: a unique key, or the grant
// limit that the other request used up while this one waited for the grant
// row lock.
func (s *Server) writeCreateError(ctx context.Context, rw http.ResponseWriter, req createRequest, err error) {
	resp, done, lookupErr := s.existingCreateOperation(ctx, s.db, req)
	if done {
		httpapi.Write(ctx, rw, http.StatusOK, resp)
		return
	}
	if lookupErr != nil {
		var ae *apiError
		if errors.As(lookupErr, &ae) {
			// The operation created another workspace.
			s.writeErr(ctx, rw, lookupErr)
			return
		}
		s.logger.Warn(ctx, "look up create operation after a failed create",
			slog.F("operation_id", req.body.OperationID), slog.Error(lookupErr))
	}
	s.writeErr(ctx, rw, createError(err))
}

// createError maps an error of the Coder create path to an admission API
// error.
func createError(err error) error {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, workspaceadmission.ErrOwnerInactive):
		return newAPIError(http.StatusForbidden, CodeOwnerInactive, "The workspace owner is not an active user.")
	case database.IsUniqueViolation(err, database.UniqueWorkspacesOwnerIDLowerIndex):
		return newAPIError(http.StatusConflict, CodeNameConflict, "The owner already has a workspace with this name.")
	}
	responder, ok := httperror.IsResponder(err)
	if !ok {
		return err
	}
	status, resp := responder.Response()
	message := resp.Message
	if message == "" {
		message = http.StatusText(status)
	}
	var mapped *apiError
	switch status {
	case http.StatusConflict:
		mapped = newAPIError(status, CodeNameConflict, message)
	case http.StatusForbidden:
		mapped = newAPIError(status, CodeNotEntitled, message)
	case http.StatusNotFound:
		mapped = newAPIError(status, CodeNotFound, message)
	case http.StatusBadRequest:
		mapped = newAPIError(status, CodeInvalidRequest, message)
	default:
		return err
	}
	mapped.resp.Detail = resp.Detail
	for _, v := range resp.Validations {
		mapped.resp.Detail = strings.TrimSpace(mapped.resp.Detail + " " + v.Field + ": " + v.Detail)
	}
	return mapped
}

// deleteWorkspace serves POST /v1/workspaces/{id}/delete. It creates a
// DELETE build initiated by the owner. A pending START that no provisioner
// acquired is canceled in the same transaction; a build that a provisioner
// runs fails the request with active_build. Under a grant, the grant must
// be live, belong to the owner, allow deletes and have created the
// workspace. Requests are idempotent per operation_id and answer 202 with
// the DELETE build.
func (s *Server) deleteWorkspace(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := uuidParam(r, "workspace_id")
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	var body DeleteWorkspaceRequest
	if err := decodeBody(rw, r, &body); err != nil {
		s.writeErr(ctx, rw, err)
		return
	}
	if body.OperationID == uuid.Nil {
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, "Field operation_id is required.", "")
		return
	}
	if body.GrantID != "" && !controllerIDPattern.MatchString(body.GrantID) {
		writeError(ctx, rw, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("Field grant_id must match %s.", controllerIDPattern), "")
		return
	}

	var (
		resp      DeleteWorkspaceResponse
		deleteJob *database.ProvisionerJob
		canceled  uuid.NullUUID
		ownerID   uuid.UUID
	)
	err = s.db.InTx(func(tx database.Store) error {
		resp = DeleteWorkspaceResponse{}
		deleteJob = nil
		canceled = uuid.NullUUID{}

		existing, done, err := existingDeleteOperation(ctx, tx, body.OperationID, workspaceID)
		if err != nil || done {
			resp = existing
			return err
		}

		workspace, err := managedLiveWorkspace(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		ownerID = workspace.OwnerID
		if body.GrantID != "" {
			if err := s.checkDeleteGrant(ctx, tx, body.GrantID, workspace); err != nil {
				return err
			}
		}

		canceled, err = endUnacquiredStart(ctx, tx, workspaceID)
		if err != nil {
			return err
		}

		// The owner, or an agent under the owner's grant, asked for the
		// delete, so the owner initiates it. The build itself runs as the
		// lifecycle executor subject, which may delete any workspace, so a
		// delete also works for owners who lost access to the template.
		builder := wsbuilder.New(workspace, database.WorkspaceTransitionDelete, *s.buildUsageChecker.Load()).
			Experiments(s.experiments).
			DeploymentValues(s.deploymentValues).
			BuildMetrics(s.buildMetrics)
		//nolint:gocritic // Platform DELETE builds run as the lifecycle executor subject, like platform STOP builds.
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
			return xerrors.Errorf("build delete: %w", err)
		}
		deleteJob = job

		_, err = tx.InsertWorkspaceAdmissionDeleteOperation(ctx, database.InsertWorkspaceAdmissionDeleteOperationParams{
			OperationID:      body.OperationID,
			WorkspaceID:      workspaceID,
			WorkspaceBuildID: build.ID,
			JobID:            job.ID,
			OrganizationID:   workspace.OrganizationID,
			GrantID:          sql.NullString{String: body.GrantID, Valid: body.GrantID != ""},
			CanceledJobID:    canceled,
			CreatedAt:        dbtime.Now(),
		})
		if err != nil {
			return xerrors.Errorf("insert delete operation: %w", err)
		}
		resp = DeleteWorkspaceResponse{BuildID: build.ID, JobID: job.ID}
		return nil
	}, nil)
	if err != nil {
		// A retry may observe the winning DELETE as an active build after
		// its operation lookup, including while waiting for a grant lock.
		existing, done, lookupErr := existingDeleteOperation(ctx, s.db, body.OperationID, workspaceID)
		switch {
		case lookupErr != nil:
			err = lookupErr
		case done:
			resp = existing
			deleteJob = nil
			canceled = uuid.NullUUID{}
			err = nil
		case database.IsUniqueViolation(err, database.UniqueWorkspaceAdmissionDeleteOperationsPkey),
			database.IsUniqueViolation(err, database.UniqueWorkspaceBuildsWorkspaceIDBuildNumberKey):
			err = newAPIError(http.StatusConflict, CodeActiveBuild, "Another build was created concurrently. Retry the request.")
		}
	}
	if err != nil {
		s.writeErr(ctx, rw, err)
		return
	}

	if deleteJob != nil {
		if err := provisionerjobs.PostJob(s.pubsub, *deleteJob); err != nil {
			s.logger.Warn(ctx, "failed to post delete provisioner job", slog.F("job_id", deleteJob.ID), slog.Error(err))
		}
		if canceled.Valid {
			s.publish(ctx, canceled.UUID)
		}
		s.publishWorkspaceChanged(ctx, ownerID, workspaceID)
		s.logger.Info(ctx, "workspace admission deleted a workspace on behalf of its owner",
			slog.F("operation_id", body.OperationID),
			slog.F("workspace_id", workspaceID),
			slog.F("grant_id", body.GrantID),
		)
	}
	httpapi.Write(ctx, rw, http.StatusAccepted, resp)
}

// checkDeleteGrant checks the grant mirror of a delete: the grant is live,
// belongs to the owner, allows deletes and created the workspace.
func (s *Server) checkDeleteGrant(ctx context.Context, tx database.Store, grantID string, workspace database.Workspace) error {
	grant, err := s.lockLiveGrant(ctx, tx, grantID, workspace.OwnerID)
	if err != nil {
		return err
	}
	if !grant.AllowDelete {
		return newAPIError(http.StatusForbidden, CodeGrantScope, "The grant does not allow deletes.")
	}
	op, err := tx.GetWorkspaceAdmissionCreateOperationByWorkspaceID(ctx, workspace.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return xerrors.Errorf("get create operation of workspace: %w", err)
	}
	if err != nil || op.GrantID.String != grant.ID {
		return newAPIError(http.StatusForbidden, CodeGrantScope, "The grant can only delete workspaces created under it.")
	}
	return nil
}

// existingDeleteOperation returns the DELETE build of an operation that
// already exists. It reports done when the operation exists for this
// workspace.
func existingDeleteOperation(ctx context.Context, db database.Store, operationID, workspaceID uuid.UUID) (DeleteWorkspaceResponse, bool, error) {
	op, err := db.GetWorkspaceAdmissionDeleteOperationByOperationID(ctx, operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteWorkspaceResponse{}, false, nil
	}
	if err != nil {
		return DeleteWorkspaceResponse{}, false, xerrors.Errorf("get delete operation: %w", err)
	}
	if op.WorkspaceID != workspaceID {
		return DeleteWorkspaceResponse{}, false, newAPIError(http.StatusConflict, CodeOperationConflict, "The operation already deleted another workspace.")
	}
	return DeleteWorkspaceResponse{BuildID: op.WorkspaceBuildID, JobID: op.JobID}, true, nil
}
