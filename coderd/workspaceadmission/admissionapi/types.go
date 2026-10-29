package admissionapi

import (
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/coder/coder/v2/coderd/database"
)

// Error codes returned in ErrorResponse.Code.
const (
	CodeUnauthorized        = "unauthorized"
	CodeTokenUnavailable    = "token_unavailable" //nolint:gosec // An error code, not a credential.
	CodeInvalidRequest      = "invalid_request"
	CodeNotFound            = "not_found"
	CodeInternal            = "internal_error"
	CodeProfileMismatch     = "profile_mismatch"
	CodeAlreadyAcquired     = "already_acquired"
	CodeAlreadyGranted      = "already_granted"
	CodeStaleGeneration     = "stale_generation"
	CodeReservationMismatch = "reservation_mismatch"
	CodeJobNotPending       = "job_not_pending"
	CodeInvalidStatus       = "invalid_status"
	CodeTemplateNotManaged  = "template_not_managed"
	CodeSystemUser          = "system_user"
	CodeActiveBuild         = "active_build"
	CodeWorkspaceDeleted    = "workspace_deleted"
	CodeOperationConflict   = "operation_conflict"
	CodeJobCompleted        = "job_completed"
	CodeJobCanceling        = "job_canceling"
	// CodeNameConflict means the owner already has a workspace with the
	// requested name.
	CodeNameConflict = "name_conflict"
	// CodeOwnerInactive means the workspace owner is suspended, dormant,
	// deleted or a system user.
	CodeOwnerInactive = "owner_inactive"
	// CodeNotEntitled means the owner may not create the workspace: no
	// entitlement for the template and profile, or no permission to use
	// the template in its organization.
	CodeNotEntitled = "not_entitled"
	// CodeGrantNotFound means the named grant was never mirrored.
	CodeGrantNotFound = "grant_not_found"
	// CodeGrantRevoked means the named grant was revoked.
	CodeGrantRevoked = "grant_revoked"
	// CodeGrantExpired means the named grant expired.
	CodeGrantExpired = "grant_expired"
	// CodeGrantScope means the grant does not cover the request: another
	// owner, organization or template, a name without the grant's prefix,
	// a delete without allow_delete or of a workspace created otherwise.
	CodeGrantScope = "grant_scope"
	// CodeGrantQuota means the grant already has max_workspaces live
	// workspaces.
	CodeGrantQuota = "grant_quota"
)

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	// Request is the current state of the admission request, when the error
	// concerns one.
	Request *Request `json:"request,omitempty"`
	// Job is the current state of the conflicting build job, when the error
	// concerns one.
	Job *BuildJob `json:"job,omitempty"`
}

// JobState is the state of a provisioner job.
type JobState struct {
	// Status is one of pending, running, succeeded, failed, canceling or
	// canceled.
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	CanceledAt  *time.Time `json:"canceled_at"`
	Error       string     `json:"error"`
}

// Request is a workspace START admission request.
type Request struct {
	JobID             uuid.UUID  `json:"job_id"`
	WorkspaceBuildID  uuid.UUID  `json:"workspace_build_id"`
	WorkspaceID       uuid.UUID  `json:"workspace_id"`
	OrganizationID    uuid.UUID  `json:"organization_id"`
	OwnerID           uuid.UUID  `json:"owner_id"`
	TemplateID        uuid.UUID  `json:"template_id"`
	TemplateVersionID uuid.UUID  `json:"template_version_id"`
	Profile           string     `json:"profile"`
	Status            string     `json:"status"`
	Reason            string     `json:"reason"`
	QueuePosition     *int32     `json:"queue_position"`
	OperationID       *uuid.UUID `json:"operation_id"`
	ReservationID     *uuid.UUID `json:"reservation_id"`
	Generation        *int64     `json:"generation"`
	GrantExpiresAt    *time.Time `json:"grant_expires_at"`
	QueuedAt          time.Time  `json:"queued_at"`
	GrantedAt         *time.Time `json:"granted_at"`
	AcquiredAt        *time.Time `json:"acquired_at"`
	DecidedAt         *time.Time `json:"decided_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	Job               JobState   `json:"job"`
	WorkspaceDeleted  bool       `json:"workspace_deleted"`
}

// ListRequestsResponse is the body of GET /v1/requests.
type ListRequestsResponse struct {
	Requests []Request `json:"requests"`
	// NextAfter is the cursor for the next page, or null when this page is
	// the last one.
	NextAfter *uuid.UUID `json:"next_after"`
}

// GrantRequest is the body of POST /v1/requests/{job_id}/grant.
type GrantRequest struct {
	ReservationID   uuid.UUID `json:"reservation_id"`
	Generation      int64     `json:"generation"`
	OperationID     uuid.UUID `json:"operation_id"`
	Profile         string    `json:"profile"`
	GrantTTLSeconds int64     `json:"grant_ttl_seconds"`
}

// RevokeRequest is the body of POST /v1/requests/{job_id}/revoke.
type RevokeRequest struct {
	ReservationID uuid.UUID `json:"reservation_id"`
	Reason        string    `json:"reason"`
}

// RevokeResponse is the body of a successful revoke.
type RevokeResponse struct {
	// Acquired is true when a provisioner already acquired the job, in which
	// case the admission did not change.
	Acquired bool    `json:"acquired"`
	Request  Request `json:"request"`
}

// RejectRequest is the body of POST /v1/requests/{job_id}/reject.
type RejectRequest struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// DisplayUpdate is one entry of POST /v1/requests/display.
type DisplayUpdate struct {
	JobID         uuid.UUID `json:"job_id"`
	Reason        string    `json:"reason"`
	QueuePosition *int32    `json:"queue_position"`
}

// DisplayResponse is the body of a successful display update.
type DisplayResponse struct {
	// Updated lists the waiting requests that changed.
	Updated []uuid.UUID `json:"updated"`
	// Skipped lists requests that are missing or no longer waiting.
	Skipped []uuid.UUID `json:"skipped"`
}

// WatchResponse is the body of GET /v1/watch.
type WatchResponse struct {
	Cursor string `json:"cursor"`
	// Changed is true when admission records may have changed since the
	// cursor the caller sent.
	Changed bool `json:"changed"`
}

// Template is a template managed by workspace admission.
type Template struct {
	TemplateID       uuid.UUID `json:"template_id"`
	OrganizationID   uuid.UUID `json:"organization_id"`
	ProfileParameter string    `json:"profile_parameter"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ListTemplatesResponse is the body of GET /v1/templates.
type ListTemplatesResponse struct {
	Templates []Template `json:"templates"`
}

// PutTemplateRequest is the body of PUT /v1/templates/{template_id}.
type PutTemplateRequest struct {
	ProfileParameter string `json:"profile_parameter"`
}

// DeleteTemplateResponse is the body of DELETE /v1/templates/{template_id}.
type DeleteTemplateResponse struct {
	Deleted bool `json:"deleted"`
}

// TemplateEntitlement lists the profiles a user may start on a template.
type TemplateEntitlement struct {
	TemplateID      uuid.UUID `json:"template_id"`
	AllowedProfiles []string  `json:"allowed_profiles"`
}

// Entitlements are all template entitlements of one user.
type Entitlements struct {
	UserID    uuid.UUID             `json:"user_id"`
	Templates []TemplateEntitlement `json:"templates"`
}

// PutEntitlementsRequest is the body of PUT /v1/entitlements/{user_id}. It
// replaces every entitlement of the user.
type PutEntitlementsRequest struct {
	Templates []TemplateEntitlement `json:"templates"`
}

// User is a Coder user as seen by the admission service.
type User struct {
	ID               uuid.UUID `json:"id"`
	Username         string    `json:"username"`
	Email            string    `json:"email"`
	Name             string    `json:"name"`
	Status           string    `json:"status"`
	LoginType        string    `json:"login_type"`
	Deleted          bool      `json:"deleted"`
	IsSystem         bool      `json:"is_system"`
	IsServiceAccount bool      `json:"is_service_account"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	// OIDCIssuer and OIDCSubject identify the linked OIDC account, if any.
	OIDCIssuer  string `json:"oidc_issuer,omitempty"`
	OIDCSubject string `json:"oidc_subject,omitempty"`
}

// ListUsersResponse is the body of GET /v1/users.
type ListUsersResponse struct {
	Users []User `json:"users"`
}

// UserStatusRequest is the body of the suspend and activate endpoints.
type UserStatusRequest struct {
	Reason string `json:"reason"`
}

// BuildJob is a workspace build and its job.
type BuildJob struct {
	BuildID     uuid.UUID `json:"build_id"`
	BuildNumber int32     `json:"build_number"`
	Transition  string    `json:"transition"`
	CreatedAt   time.Time `json:"created_at"`
	JobID       uuid.UUID `json:"job_id"`
	Job         JobState  `json:"job"`
	// AdmissionStatus is the admission status of a START build of a managed
	// template, or empty for builds without admission.
	AdmissionStatus string `json:"admission_status,omitempty"`
}

// Workspace is a workspace of a managed template.
type Workspace struct {
	ID             uuid.UUID `json:"id"`
	OrganizationID uuid.UUID `json:"organization_id"`
	OwnerID        uuid.UUID `json:"owner_id"`
	TemplateID     uuid.UUID `json:"template_id"`
	Name           string    `json:"name"`
	Deleted        bool      `json:"deleted"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	LatestBuild    BuildJob  `json:"latest_build"`
	// Agents are the top-level agents of the latest build with their
	// connection state as recorded in the database. The admission
	// service never dials agents. Omitted when the latest build has
	// none, for example after a STOP.
	Agents []WorkspaceAgent `json:"agents,omitempty"`
}

// WorkspaceAgent is the connection state of a workspace agent, computed
// from its database row the way the Coder API computes it.
type WorkspaceAgent struct {
	ID uuid.UUID `json:"id"`
	// Status is connected, connecting, disconnected or timeout.
	Status string `json:"status"`
	// LastConnectedAt is the latest time the agent reported its
	// connection to coderd.
	LastConnectedAt *time.Time `json:"last_connected_at,omitempty"`
	// DisconnectedAt is when the agent disconnected. For an agent that
	// stopped reporting without disconnecting, such as one in a paused
	// microVM, it is LastConnectedAt once the inactivity timeout passed.
	DisconnectedAt *time.Time `json:"disconnected_at,omitempty"`
}

// ListWorkspacesResponse is the body of GET /v1/workspaces.
type ListWorkspacesResponse struct {
	Workspaces []Workspace `json:"workspaces"`
}

// StopWorkspaceRequest is the body of POST /v1/workspaces/{id}/stop.
type StopWorkspaceRequest struct {
	OperationID uuid.UUID `json:"operation_id"`
	Reason      string    `json:"reason"`
}

// StopWorkspaceResponse is the body of a successful stop request.
type StopWorkspaceResponse struct {
	OperationID uuid.UUID `json:"operation_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	// Created is false when the operation already existed and the original
	// STOP build is returned.
	Created bool     `json:"created"`
	Build   BuildJob `json:"build"`
	// CanceledJobID is the pending START job that was canceled before any
	// provisioner acquired it, if any.
	CanceledJobID *uuid.UUID `json:"canceled_job_id"`
}

// CancelJobResponse is the body of POST /v1/jobs/{job_id}/cancel.
type CancelJobResponse struct {
	JobID uuid.UUID `json:"job_id"`
	// CanceledBeforeStart is true when no provisioner had acquired the job,
	// so it was canceled immediately. Otherwise the provisioner running it
	// performs the cancellation.
	CanceledBeforeStart bool     `json:"canceled_before_start"`
	Job                 JobState `json:"job"`
}

// Kinds of create sources (CreateWorkspaceSource.Kind).
const (
	// CreateSourceEmployee is a workspace the owner asked for.
	CreateSourceEmployee = "employee"
	// CreateSourceAgentGrant is a workspace an agent created under a grant
	// of the owner.
	CreateSourceAgentGrant = "agent_grant"
)

// RichParameterValue is one build parameter of a new workspace.
type RichParameterValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CreateWorkspaceSource attributes a workspace created on behalf of its
// owner. Empty strings mean not set. Grant and Run IDs are opaque
// controller IDs; workspace IDs are UUIDs.
type CreateWorkspaceSource struct {
	// Kind is employee or agent_grant. An agent_grant create names its
	// grant.
	Kind                 string `json:"kind"`
	GrantID              string `json:"grant_id"`
	RunID                string `json:"run_id"`
	Harness              string `json:"harness"`
	InitiatorWorkspaceID string `json:"initiator_workspace_id"`
	CloneOfWorkspaceID   string `json:"clone_of_workspace_id"`
}

// CreateWorkspaceRequest is the body of POST /v1/workspaces. Coder creates
// the workspace and its START build on behalf of the owner, with the
// owner's permissions, in one transaction that also records the waiting
// admission of the build. Requests are idempotent per operation_id.
type CreateWorkspaceRequest struct {
	OperationID uuid.UUID `json:"operation_id"`
	OwnerID     uuid.UUID `json:"owner_id"`
	TemplateID  uuid.UUID `json:"template_id"`
	// TemplateVersionID is a version of the template, or empty for the
	// active version.
	TemplateVersionID   string               `json:"template_version_id"`
	Name                string               `json:"name"`
	RichParameterValues []RichParameterValue `json:"rich_parameter_values,omitempty"`
	// Profile is the requested profile. The template's profile parameter
	// is set to it when the parameters do not set it, and must equal it
	// when they do.
	Profile string                `json:"profile"`
	Source  CreateWorkspaceSource `json:"source"`
}

// CreateWorkspaceResponse is the body of a successful create. Created is
// false when the operation already existed and the original workspace is
// returned.
type CreateWorkspaceResponse struct {
	Created     bool      `json:"created"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	BuildID     uuid.UUID `json:"build_id"`
	JobID       uuid.UUID `json:"job_id"`
	// Admission is the admission request of the START build, waiting for
	// capacity when the workspace was just created.
	Admission Request `json:"admission"`
}

// DeleteWorkspaceRequest is the body of POST /v1/workspaces/{id}/delete.
// A delete under a grant needs a grant with allow_delete that created the
// workspace.
type DeleteWorkspaceRequest struct {
	OperationID uuid.UUID `json:"operation_id"`
	// GrantID is the grant the delete runs under, or empty for a delete the
	// owner asked for.
	GrantID string `json:"grant_id"`
}

// DeleteWorkspaceResponse is the DELETE build of a delete request. Repeats
// of the operation return the original build.
type DeleteWorkspaceResponse struct {
	BuildID uuid.UUID `json:"build_id"`
	JobID   uuid.UUID `json:"job_id"`
}

// GrantMirror is the scope of a controller grant that Coder checks, as a
// backstop, when the controller creates or deletes workspaces under it. The
// controller is authoritative for grants and counts their workspaces.
type GrantMirror struct {
	ID             string    `json:"id"`
	OwnerID        uuid.UUID `json:"owner_id"`
	OrganizationID uuid.UUID `json:"organization_id"`
	// TemplateIDs limits the templates of new workspaces. Empty allows
	// every template the owner is entitled to.
	TemplateIDs []uuid.UUID `json:"template_ids,omitempty"`
	// NamePrefix must start the name of every new workspace when set.
	NamePrefix string `json:"name_prefix"`
	// MaxWorkspaces bounds the workspaces created under the grant that are
	// not deleted, including creates in progress.
	MaxWorkspaces int  `json:"max_workspaces"`
	AllowDelete   bool `json:"allow_delete"`
	// ExpiresAt ends the grant. It is required.
	ExpiresAt time.Time `json:"expires_at"`
	// RevokedAt is set once the grant was revoked. A revocation is final.
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Results of an acknowledged wake intent.
const (
	// WakeResultWoken means the controller woke the workspace.
	WakeResultWoken = "woken"
	// WakeResultAlreadyRunning means the workspace was already running.
	WakeResultAlreadyRunning = "already_running"
	// WakeResultFailed means the wake failed.
	WakeResultFailed = "failed"
	// WakeResultIgnored means the controller does not wake this workspace,
	// for example because its pool has no power management.
	WakeResultIgnored = "ignored"
)

// WakeIntent is a request to wake a managed workspace, recorded when a
// client connected to one of its agents. It is pending while AckedAt is
// null.
type WakeIntent struct {
	WorkspaceID uuid.UUID `json:"workspace_id"`
	// AgentID is the agent the latest request tried to reach.
	AgentID uuid.UUID `json:"agent_id"`
	// Source is the client path of the latest request: tailnet, cli, pty
	// or app.
	Source string `json:"source"`
	// RequestedBy is the user ID of the latest request, or empty when the
	// requester was not a user.
	RequestedBy string `json:"requested_by"`
	// FirstRequestedAt is the first request since the last
	// acknowledgement.
	FirstRequestedAt time.Time `json:"first_requested_at"`
	// LastRequestedAt identifies the latest request. Acknowledgements must
	// echo it unchanged.
	LastRequestedAt time.Time `json:"last_requested_at"`
	// RequestCount counts the requests since the last acknowledgement.
	RequestCount    int        `json:"request_count"`
	AckedAt         *time.Time `json:"acked_at,omitempty"`
	AckedPowerEpoch *int64     `json:"acked_power_epoch,omitempty"`
}

// ListWakeIntentsResponse is the body of GET /v1/wake-intents.
type ListWakeIntentsResponse struct {
	Intents []WakeIntent `json:"intents"`
}

// AckWakeIntentRequest is the body of POST
// /v1/wake-intents/{workspace_id}/ack.
type AckWakeIntentRequest struct {
	// LastRequestedAt is the latest request the controller acted on.
	LastRequestedAt time.Time `json:"last_requested_at"`
	// PowerEpoch is the controller power epoch after the wake.
	PowerEpoch int64 `json:"power_epoch"`
	// Result is woken, already_running, failed or ignored.
	Result string `json:"result"`
}

// AckIntentResponse is the body of an intent acknowledgement. Acked is false
// when a newer request arrived or the workspace has no intent.
type AckIntentResponse struct {
	Acked bool `json:"acked"`
}

// Results of an acknowledged keepalive intent.
const (
	// KeepaliveResultApplied means the controller kept the workspace awake
	// until the acknowledged end.
	KeepaliveResultApplied = "applied"
	// KeepaliveResultDenied means the controller refused the keepalive.
	KeepaliveResultDenied = "denied"
)

// KeepaliveIntent is a request to keep a managed workspace awake, recorded
// when a user asked for it, for example from the sleep notice on the
// workspace page. It is pending while AckedAt is null.
type KeepaliveIntent struct {
	WorkspaceID uuid.UUID `json:"workspace_id"`
	// RequestedBy is the user ID of the latest request.
	RequestedBy uuid.UUID `json:"requested_by"`
	// RequestedAt identifies the latest request. Acknowledgements must echo
	// it unchanged.
	RequestedAt time.Time `json:"requested_at"`
	// ForSeconds is the requested duration. The controller caps it at its
	// own maximum.
	ForSeconds int        `json:"for_seconds"`
	AckedAt    *time.Time `json:"acked_at,omitempty"`
}

// ListKeepaliveIntentsResponse is the body of GET /v1/keepalive-intents.
type ListKeepaliveIntentsResponse struct {
	Intents []KeepaliveIntent `json:"intents"`
}

// AckKeepaliveIntentRequest is the body of POST
// /v1/keepalive-intents/{workspace_id}/ack.
type AckKeepaliveIntentRequest struct {
	// RequestedAt is the latest request the controller acted on.
	RequestedAt time.Time `json:"requested_at"`
	// Until is the keepalive end the controller applied. It is required
	// when Result is applied and ignored when it is denied, where it may be
	// absent or the zero time.
	Until *time.Time `json:"until,omitempty"`
	// Result is applied or denied.
	Result string `json:"result"`
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func uuidPtr(u uuid.NullUUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	v := u.UUID
	return &v
}

func convertJobState(status database.ProvisionerJobStatus, createdAt time.Time, startedAt, completedAt, canceledAt sql.NullTime, jobErr sql.NullString) JobState {
	return JobState{
		Status:      string(status),
		CreatedAt:   createdAt,
		StartedAt:   timePtr(startedAt),
		CompletedAt: timePtr(completedAt),
		CanceledAt:  timePtr(canceledAt),
		Error:       jobErr.String,
	}
}

func convertProvisionerJobState(job database.ProvisionerJob) JobState {
	return convertJobState(job.JobStatus, job.CreatedAt, job.StartedAt, job.CompletedAt, job.CanceledAt, job.Error)
}

func convertRequest(a database.WorkspaceBuildAdmission, job JobState, workspaceDeleted bool) Request {
	r := Request{
		JobID:             a.JobID,
		WorkspaceBuildID:  a.WorkspaceBuildID,
		WorkspaceID:       a.WorkspaceID,
		OrganizationID:    a.OrganizationID,
		OwnerID:           a.OwnerID,
		TemplateID:        a.TemplateID,
		TemplateVersionID: a.TemplateVersionID,
		Profile:           a.Profile,
		Status:            string(a.Status),
		Reason:            a.Reason,
		OperationID:       uuidPtr(a.OperationID),
		ReservationID:     uuidPtr(a.ReservationID),
		GrantExpiresAt:    timePtr(a.GrantExpiresAt),
		QueuedAt:          a.QueuedAt,
		GrantedAt:         timePtr(a.GrantedAt),
		AcquiredAt:        timePtr(a.AcquiredAt),
		DecidedAt:         timePtr(a.DecidedAt),
		UpdatedAt:         a.UpdatedAt,
		Job:               job,
		WorkspaceDeleted:  workspaceDeleted,
	}
	if a.QueuePosition.Valid {
		v := a.QueuePosition.Int32
		r.QueuePosition = &v
	}
	if a.Generation.Valid {
		v := a.Generation.Int64
		r.Generation = &v
	}
	return r
}

func convertRequestRow(row database.GetWorkspaceBuildAdmissionRequestByJobIDRow) Request {
	return convertRequest(
		row.WorkspaceBuildAdmission,
		convertJobState(row.JobStatus, row.JobCreatedAt, row.JobStartedAt, row.JobCompletedAt, row.JobCanceledAt, row.JobError),
		row.WorkspaceDeleted,
	)
}

func convertRequestListRow(row database.GetWorkspaceBuildAdmissionRequestsRow) Request {
	return convertRequest(
		row.WorkspaceBuildAdmission,
		convertJobState(row.JobStatus, row.JobCreatedAt, row.JobStartedAt, row.JobCompletedAt, row.JobCanceledAt, row.JobError),
		row.WorkspaceDeleted,
	)
}

func convertTemplate(t database.WorkspaceAdmissionTemplate) Template {
	return Template{
		TemplateID:       t.TemplateID,
		OrganizationID:   t.OrganizationID,
		ProfileParameter: t.ProfileParameter,
		CreatedAt:        t.CreatedAt,
		UpdatedAt:        t.UpdatedAt,
	}
}

func convertEntitlements(userID uuid.UUID, rows []database.WorkspaceAdmissionEntitlement) Entitlements {
	e := Entitlements{UserID: userID, Templates: make([]TemplateEntitlement, 0, len(rows))}
	for _, row := range rows {
		profiles := row.AllowedProfiles
		if profiles == nil {
			profiles = []string{}
		}
		e.Templates = append(e.Templates, TemplateEntitlement{
			TemplateID:      row.TemplateID,
			AllowedProfiles: profiles,
		})
	}
	return e
}

// parseOIDCLinkedID splits a user link ID that Coder stores as
// "issuer||subject" for OIDC users. Links without the separator only carry
// the subject.
func parseOIDCLinkedID(linkedID string) (issuer, subject string) {
	issuer, subject, found := strings.Cut(linkedID, "||")
	if !found {
		return "", linkedID
	}
	return issuer, subject
}

func convertUser(row database.GetWorkspaceAdmissionUsersRow) User {
	u := User{
		ID:               row.ID,
		Username:         row.Username,
		Email:            row.Email,
		Name:             row.Name,
		Status:           string(row.Status),
		LoginType:        string(row.LoginType),
		Deleted:          row.Deleted,
		IsSystem:         row.IsSystem,
		IsServiceAccount: row.IsServiceAccount,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
	if row.OidcLinkedID != "" {
		u.OIDCIssuer, u.OIDCSubject = parseOIDCLinkedID(row.OidcLinkedID)
	}
	return u
}

func convertWorkspace(row database.GetWorkspaceAdmissionWorkspacesRow) Workspace {
	w := Workspace{
		ID:             row.ID,
		OrganizationID: row.OrganizationID,
		OwnerID:        row.OwnerID,
		TemplateID:     row.TemplateID,
		Name:           row.Name,
		Deleted:        row.Deleted,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
		LatestBuild: BuildJob{
			BuildID:     row.LatestBuildID,
			BuildNumber: row.LatestBuildNumber,
			Transition:  string(row.LatestBuildTransition),
			CreatedAt:   row.LatestBuildCreatedAt,
			JobID:       row.LatestJobID,
			Job: convertJobState(row.LatestJobStatus, row.LatestJobCreatedAt, row.LatestJobStartedAt,
				row.LatestJobCompletedAt, row.LatestJobCanceledAt, row.LatestJobError),
		},
	}
	if row.LatestAdmissionStatus.Valid {
		w.LatestBuild.AdmissionStatus = string(row.LatestAdmissionStatus.WorkspaceAdmissionStatus)
	}
	return w
}

func convertWorkspaceAgent(agent database.WorkspaceAgent, now time.Time, inactiveTimeout time.Duration) WorkspaceAgent {
	status := agent.Status(now, inactiveTimeout)
	return WorkspaceAgent{
		ID:              agent.ID,
		Status:          string(status.Status),
		LastConnectedAt: status.LastConnectedAt,
		DisconnectedAt:  status.DisconnectedAt,
	}
}

func convertBuildJob(build database.WorkspaceBuild, job database.ProvisionerJob, admission *database.WorkspaceBuildAdmission) BuildJob {
	b := BuildJob{
		BuildID:     build.ID,
		BuildNumber: build.BuildNumber,
		Transition:  string(build.Transition),
		CreatedAt:   build.CreatedAt,
		JobID:       job.ID,
		Job:         convertProvisionerJobState(job),
	}
	if admission != nil {
		b.AdmissionStatus = string(admission.Status)
	}
	return b
}

func convertWakeIntent(row database.WorkspaceAdmissionWakeIntent) WakeIntent {
	i := WakeIntent{
		WorkspaceID:      row.WorkspaceID,
		AgentID:          row.AgentID,
		Source:           row.Source,
		FirstRequestedAt: row.FirstRequestedAt,
		LastRequestedAt:  row.LastRequestedAt,
		RequestCount:     int(row.RequestCount),
		AckedAt:          timePtr(row.AckedAt),
	}
	if row.RequestedBy.Valid {
		i.RequestedBy = row.RequestedBy.UUID.String()
	}
	if row.AckedPowerEpoch.Valid {
		v := row.AckedPowerEpoch.Int64
		i.AckedPowerEpoch = &v
	}
	return i
}

func convertKeepaliveIntent(row database.WorkspaceAdmissionKeepaliveIntent) KeepaliveIntent {
	return KeepaliveIntent{
		WorkspaceID: row.WorkspaceID,
		RequestedBy: row.RequestedBy,
		RequestedAt: row.RequestedAt,
		ForSeconds:  int(row.ForSeconds),
		AckedAt:     timePtr(row.AckedAt),
	}
}

func convertGrant(row database.WorkspaceAdmissionGrant) GrantMirror {
	return GrantMirror{
		ID:             row.ID,
		OwnerID:        row.OwnerID,
		OrganizationID: row.OrganizationID,
		TemplateIDs:    row.TemplateIDs,
		NamePrefix:     row.NamePrefix,
		MaxWorkspaces:  int(row.MaxWorkspaces),
		AllowDelete:    row.AllowDelete,
		ExpiresAt:      row.ExpiresAt,
		RevokedAt:      timePtr(row.RevokedAt),
	}
}
