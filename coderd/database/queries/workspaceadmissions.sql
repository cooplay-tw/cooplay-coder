-- name: GetWorkspaceAdmissionTemplateByTemplateID :one
SELECT
	*
FROM
	workspace_admission_templates
WHERE
	template_id = @template_id;

-- name: GetWorkspaceAdmissionTemplates :many
SELECT
	*
FROM
	workspace_admission_templates
ORDER BY
	created_at, template_id;

-- name: UpsertWorkspaceAdmissionTemplate :one
-- Marks a template as managed by workspace admission. The organization is
-- copied from the template so callers cannot attach a template to another
-- organization. Returns no rows when the template does not exist or is
-- deleted.
INSERT INTO workspace_admission_templates (
	template_id,
	organization_id,
	profile_parameter,
	created_at,
	updated_at
)
SELECT
	t.id,
	t.organization_id,
	@profile_parameter::text,
	@now::timestamptz,
	@now::timestamptz
FROM
	templates t
WHERE
	t.id = @template_id
	AND NOT t.deleted
ON CONFLICT (template_id) DO UPDATE SET
	profile_parameter = EXCLUDED.profile_parameter,
	updated_at = EXCLUDED.updated_at
RETURNING
	*;

-- name: DeleteWorkspaceAdmissionTemplateByTemplateID :execrows
DELETE FROM
	workspace_admission_templates
WHERE
	template_id = @template_id;

-- name: GetWorkspaceAdmissionEntitlementsByUserID :many
SELECT
	*
FROM
	workspace_admission_entitlements
WHERE
	user_id = @user_id
ORDER BY
	template_id;

-- name: GetWorkspaceAdmissionEntitlementByUserIDAndTemplateID :one
SELECT
	*
FROM
	workspace_admission_entitlements
WHERE
	user_id = @user_id
	AND template_id = @template_id;

-- name: DeleteWorkspaceAdmissionEntitlementsByUserID :exec
DELETE FROM
	workspace_admission_entitlements
WHERE
	user_id = @user_id;

-- name: InsertWorkspaceAdmissionEntitlement :one
-- Returns no rows when the template is not managed by workspace admission.
INSERT INTO workspace_admission_entitlements (
	user_id,
	template_id,
	organization_id,
	allowed_profiles,
	updated_at
)
SELECT
	@user_id::uuid,
	wat.template_id,
	wat.organization_id,
	@allowed_profiles::text[],
	@updated_at::timestamptz
FROM
	workspace_admission_templates wat
WHERE
	wat.template_id = @template_id
RETURNING
	*;

-- name: InsertWorkspaceBuildAdmission :one
-- Records that a START build on a managed template waits for capacity. Every
-- admission starts as waiting; only the admission service grants it.
INSERT INTO workspace_build_admissions (
	job_id,
	workspace_build_id,
	workspace_id,
	organization_id,
	owner_id,
	template_id,
	template_version_id,
	profile,
	status,
	reason,
	queued_at,
	updated_at
) VALUES (
	@job_id,
	@workspace_build_id,
	@workspace_id,
	@organization_id,
	@owner_id,
	@template_id,
	@template_version_id,
	@profile,
	'waiting'::workspace_admission_status,
	@reason,
	@queued_at,
	@queued_at
)
RETURNING
	*;

-- name: GetWorkspaceBuildAdmissionByJobID :one
SELECT
	*
FROM
	workspace_build_admissions
WHERE
	job_id = @job_id;

-- name: GetWorkspaceBuildAdmissionsByBuildIDs :many
SELECT
	*
FROM
	workspace_build_admissions
WHERE
	workspace_build_id = ANY(@build_ids::uuid[]);

-- name: GetWorkspaceAdmissionStateByJobID :one
-- Reports whether a provisioner job is a managed START (a workspace START
-- build whose workspace template is managed by workspace admission) and the
-- admission state of that job, if any.
SELECT
	COALESCE(
		wb.transition = 'start'::workspace_transition AND wat.template_id IS NOT NULL,
		false
	)::boolean AS managed_start,
	wba.status AS admission_status,
	wba.granted_at AS admission_granted_at
FROM
	provisioner_jobs pj
LEFT JOIN
	workspace_builds wb ON wb.job_id = pj.id
LEFT JOIN
	workspaces w ON w.id = wb.workspace_id
LEFT JOIN
	workspace_admission_templates wat ON wat.template_id = w.template_id
LEFT JOIN
	workspace_build_admissions wba ON wba.job_id = pj.id
WHERE
	pj.id = @job_id;

-- name: GetWorkspaceBuildAdmissionRequests :many
-- Lists admission requests in stable queue order for the admission service.
-- The cursor is the (queued_at, job_id) pair of the last row of the previous
-- page.
SELECT
	sqlc.embed(wba),
	pj.job_status,
	pj.created_at AS job_created_at,
	pj.started_at AS job_started_at,
	pj.completed_at AS job_completed_at,
	pj.canceled_at AS job_canceled_at,
	pj.error AS job_error,
	w.deleted AS workspace_deleted
FROM
	workspace_build_admissions wba
JOIN
	provisioner_jobs pj ON pj.id = wba.job_id
JOIN
	workspaces w ON w.id = wba.workspace_id
WHERE
	(
		COALESCE(array_length(@statuses::workspace_admission_status[], 1), 0) = 0
		OR wba.status = ANY(@statuses::workspace_admission_status[])
	)
	AND (
		NOT @has_cursor::boolean
		OR (wba.queued_at, wba.job_id) > (@after_queued_at::timestamptz, @after_job_id::uuid)
	)
ORDER BY
	wba.queued_at, wba.job_id
LIMIT
	@limit_count::int;

-- name: GetWorkspaceBuildAdmissionRequestByJobID :one
SELECT
	sqlc.embed(wba),
	pj.job_status,
	pj.created_at AS job_created_at,
	pj.started_at AS job_started_at,
	pj.completed_at AS job_completed_at,
	pj.canceled_at AS job_canceled_at,
	pj.error AS job_error,
	w.deleted AS workspace_deleted
FROM
	workspace_build_admissions wba
JOIN
	provisioner_jobs pj ON pj.id = wba.job_id
JOIN
	workspaces w ON w.id = wba.workspace_id
WHERE
	wba.job_id = @job_id;

-- name: GrantWorkspaceBuildAdmission :one
-- Grants a waiting admission, or refreshes an existing grant. granted_at keeps
-- the time of the first grant so a refresh does not extend the job reaper
-- deadline. Callers hold the provisioner job row lock.
UPDATE
	workspace_build_admissions
SET
	status = 'granted'::workspace_admission_status,
	reason = '',
	queue_position = NULL,
	reservation_id = @reservation_id::uuid,
	generation = @generation::bigint,
	operation_id = @operation_id::uuid,
	grant_expires_at = @grant_expires_at::timestamptz,
	granted_at = CASE
		WHEN status = 'granted'::workspace_admission_status THEN granted_at
		ELSE @now::timestamptz
	END,
	decided_at = @now::timestamptz,
	updated_at = @now::timestamptz
WHERE
	job_id = @job_id
	AND status IN ('waiting'::workspace_admission_status, 'granted'::workspace_admission_status)
RETURNING
	*;

-- name: RequeueWorkspaceBuildAdmission :one
-- Returns a waiting or granted admission to the capacity queue and clears the
-- grant. Callers hold the provisioner job row lock and have verified that no
-- provisioner acquired the job.
UPDATE
	workspace_build_admissions
SET
	status = 'waiting'::workspace_admission_status,
	reason = @reason,
	queue_position = NULL,
	reservation_id = NULL,
	generation = NULL,
	operation_id = NULL,
	grant_expires_at = NULL,
	granted_at = NULL,
	decided_at = @now::timestamptz,
	updated_at = @now::timestamptz
WHERE
	job_id = @job_id
	AND status IN ('waiting'::workspace_admission_status, 'granted'::workspace_admission_status)
RETURNING
	*;

-- name: UpdatePendingWorkspaceBuildAdmissionStatus :one
-- Ends an admission that no provisioner acquired, as rejected, canceled or
-- revoked. Returns no rows when the admission does not exist, was acquired or
-- already ended.
UPDATE
	workspace_build_admissions
SET
	status = @status::workspace_admission_status,
	reason = @reason,
	queue_position = NULL,
	decided_at = @now::timestamptz,
	updated_at = @now::timestamptz
WHERE
	job_id = @job_id
	AND status IN ('waiting'::workspace_admission_status, 'granted'::workspace_admission_status)
	AND @status::workspace_admission_status IN (
		'rejected'::workspace_admission_status,
		'canceled'::workspace_admission_status,
		'revoked'::workspace_admission_status
	)
RETURNING
	*;

-- name: UpdateWorkspaceBuildAdmissionDisplays :many
-- Updates the waiting reason and capacity queue position shown to users.
-- Only waiting rows change. A queue position of -1 clears the position.
-- Returns the changed rows with their workspace and owner so callers can
-- notify the workspace watchers.
UPDATE
	workspace_build_admissions wba
SET
	reason = d.reason,
	queue_position = NULLIF(d.queue_position, -1),
	updated_at = @updated_at::timestamptz
FROM (
	SELECT
		unnest(@job_ids::uuid[]) AS job_id,
		unnest(@reasons::text[]) AS reason,
		unnest(@queue_positions::integer[]) AS queue_position
) d
WHERE
	wba.job_id = d.job_id
	AND wba.status = 'waiting'::workspace_admission_status
RETURNING
	wba.job_id,
	wba.workspace_id,
	wba.owner_id;

-- name: GetWorkspaceBuildAdmissionsWatermark :one
-- Returns the latest admission update time. The admission service uses it as
-- a change cursor for long polling.
SELECT
	COALESCE(MAX(updated_at), '0001-01-01 00:00:00+00'::timestamptz)::timestamptz AS max_updated_at
FROM
	workspace_build_admissions;

-- name: GetWorkspaceAdmissionUsers :many
-- Lists users with their OIDC link for the admission service. A zero user ID
-- lists every user.
SELECT
	u.id,
	u.username,
	u.email,
	u.name,
	u.status,
	u.login_type,
	u.deleted,
	u.is_system,
	u.is_service_account,
	u.created_at,
	u.updated_at,
	COALESCE(ul.linked_id, '')::text AS oidc_linked_id
FROM
	users u
LEFT JOIN
	user_links ul ON ul.user_id = u.id AND ul.login_type = 'oidc'::login_type
WHERE
	@user_id::uuid = '00000000-0000-0000-0000-000000000000'::uuid
	OR u.id = @user_id::uuid
ORDER BY
	u.created_at, u.id;

-- name: GetWorkspaceAdmissionWorkspaces :many
-- Lists workspaces of managed templates with their latest build. A zero
-- workspace ID lists every managed workspace.
SELECT
	w.id,
	w.organization_id,
	w.owner_id,
	w.template_id,
	w.name,
	w.deleted,
	w.created_at,
	w.updated_at,
	lb.id AS latest_build_id,
	lb.build_number AS latest_build_number,
	lb.transition AS latest_build_transition,
	lb.created_at AS latest_build_created_at,
	pj.id AS latest_job_id,
	pj.job_status AS latest_job_status,
	pj.created_at AS latest_job_created_at,
	pj.started_at AS latest_job_started_at,
	pj.completed_at AS latest_job_completed_at,
	pj.canceled_at AS latest_job_canceled_at,
	pj.error AS latest_job_error,
	wba.status AS latest_admission_status
FROM
	workspaces w
JOIN
	workspace_admission_templates wat ON wat.template_id = w.template_id
JOIN
	workspace_builds lb ON lb.workspace_id = w.id
	AND lb.build_number = (
		SELECT
			MAX(b.build_number)
		FROM
			workspace_builds b
		WHERE
			b.workspace_id = w.id
	)
JOIN
	provisioner_jobs pj ON pj.id = lb.job_id
LEFT JOIN
	workspace_build_admissions wba ON wba.job_id = lb.job_id
WHERE
	@workspace_id::uuid = '00000000-0000-0000-0000-000000000000'::uuid
	OR w.id = @workspace_id::uuid
ORDER BY
	w.created_at, w.id;

-- name: UpdateManagedWorkspaceBuildJobWithCancelByID :execrows
-- Cancels a workspace build job of a managed template on behalf of the
-- admission service. Mirrors UpdateProvisionerJobWithCancelByID but only
-- matches jobs of managed workspaces.
UPDATE
	provisioner_jobs pj
SET
	canceled_at = @canceled_at,
	completed_at = @completed_at
WHERE
	pj.id = @id
	AND pj.type = 'workspace_build'::provisioner_job_type
	AND EXISTS (
		SELECT
			1
		FROM
			workspace_builds wb
		JOIN
			workspaces w ON w.id = wb.workspace_id
		JOIN
			workspace_admission_templates wat ON wat.template_id = w.template_id
		WHERE
			wb.job_id = pj.id
	);

-- name: InsertWorkspaceAdmissionStopOperation :one
INSERT INTO workspace_admission_stop_operations (
	operation_id,
	workspace_id,
	workspace_build_id,
	job_id,
	organization_id,
	canceled_job_id,
	reason,
	created_at
) VALUES (
	@operation_id,
	@workspace_id,
	@workspace_build_id,
	@job_id,
	@organization_id,
	@canceled_job_id,
	@reason,
	@created_at
)
RETURNING
	*;

-- name: GetWorkspaceAdmissionStopOperationByOperationID :one
SELECT
	*
FROM
	workspace_admission_stop_operations
WHERE
	operation_id = @operation_id;
