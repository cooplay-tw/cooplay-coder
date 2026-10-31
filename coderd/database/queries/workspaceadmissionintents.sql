-- name: GetWorkspaceAdmissionWakeTargetByAgentID :one
-- Returns what the wake intent recorder needs to decide whether a connection
-- to an agent should wake its workspace: whether the template is managed,
-- whether the agent belongs to a succeeded START that is the latest build,
-- the agent connection state and the power state the controller reported,
-- which is absent while the controller reports the workspace as off.
SELECT
	w.id AS workspace_id,
	w.organization_id,
	w.owner_id,
	w.deleted AS workspace_deleted,
	(wat.template_id IS NOT NULL)::boolean AS managed,
	(lb.id = ab.id)::boolean AS agent_in_latest_build,
	lb.transition AS latest_build_transition,
	pj.job_status AS latest_job_status,
	wss.power_state,
	sqlc.embed(wa)
FROM
	workspace_agents wa
JOIN
	workspace_resources wr ON wr.id = wa.resource_id
JOIN
	workspace_builds ab ON ab.job_id = wr.job_id
JOIN
	workspaces w ON w.id = ab.workspace_id
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
	workspace_admission_templates wat ON wat.template_id = w.template_id
LEFT JOIN
	workspace_admission_workspace_states wss ON wss.workspace_id = w.id AND wss.off_at IS NULL
WHERE
	wa.id = @agent_id;

-- name: UpsertWorkspaceAdmissionWakeIntent :one
-- Records wake requests for a workspace. The first request after an
-- acknowledgement starts a new pending intent; later requests extend it.
-- last_requested_at strictly increases so an acknowledgement that names the
-- value it saw never covers a newer request.
INSERT INTO workspace_admission_wake_intents (
	workspace_id,
	organization_id,
	agent_id,
	source,
	requested_by,
	first_requested_at,
	last_requested_at,
	request_count,
	updated_at
) VALUES (
	@workspace_id,
	@organization_id,
	@agent_id,
	@source,
	@requested_by,
	@first_requested_at,
	@last_requested_at,
	@request_count,
	clock_timestamp()
)
ON CONFLICT (workspace_id) DO UPDATE SET
	organization_id = EXCLUDED.organization_id,
	agent_id = EXCLUDED.agent_id,
	source = EXCLUDED.source,
	requested_by = EXCLUDED.requested_by,
	first_requested_at = CASE
		WHEN workspace_admission_wake_intents.acked_at IS NULL THEN workspace_admission_wake_intents.first_requested_at
		ELSE EXCLUDED.first_requested_at
	END,
	last_requested_at = GREATEST(
		EXCLUDED.last_requested_at,
		workspace_admission_wake_intents.last_requested_at + INTERVAL '1 microsecond'
	),
	request_count = CASE
		WHEN workspace_admission_wake_intents.acked_at IS NULL THEN LEAST(
			workspace_admission_wake_intents.request_count::bigint + EXCLUDED.request_count::bigint,
			2147483647
		)::integer
		ELSE EXCLUDED.request_count
	END,
	acked_at = NULL,
	acked_power_epoch = NULL,
	acked_result = NULL,
	updated_at = GREATEST(
		EXCLUDED.updated_at,
		workspace_admission_wake_intents.updated_at + INTERVAL '1 microsecond'
	)
RETURNING
	*;

-- name: GetWorkspaceAdmissionWakeIntents :many
-- Lists wake intents, oldest request first. pending_only limits the list to
-- intents whose latest request was not acknowledged.
SELECT
	*
FROM
	workspace_admission_wake_intents
WHERE
	NOT @pending_only::boolean
	OR acked_at IS NULL
ORDER BY
	first_requested_at, workspace_id
LIMIT
	@limit_count::int;

-- name: AckWorkspaceAdmissionWakeIntent :execrows
-- Acknowledges the latest wake request of a workspace. It only matches when
-- last_requested_at is the value the controller saw, so a request recorded
-- while the controller was waking the workspace stays pending. Repeating an
-- acknowledgement keeps the first acknowledgement time.
UPDATE
	workspace_admission_wake_intents
SET
	acked_at = COALESCE(acked_at, @now::timestamptz),
	acked_power_epoch = @power_epoch::bigint,
	acked_result = @result::text,
	updated_at = CASE
		WHEN acked_at IS NULL THEN GREATEST(@now::timestamptz, updated_at + INTERVAL '1 microsecond')
		ELSE updated_at
	END
WHERE
	workspace_id = @workspace_id
	AND last_requested_at = @last_requested_at::timestamptz;

-- name: UpsertWorkspaceAdmissionKeepaliveIntent :one
-- Records a keepalive request for a workspace, replacing an earlier request.
-- requested_at strictly increases so an acknowledgement that names the value
-- it saw never covers a newer request.
INSERT INTO workspace_admission_keepalive_intents (
	workspace_id,
	organization_id,
	requested_by,
	requested_at,
	for_seconds,
	updated_at
) VALUES (
	@workspace_id,
	@organization_id,
	@requested_by,
	@requested_at,
	@for_seconds,
	clock_timestamp()
)
ON CONFLICT (workspace_id) DO UPDATE SET
	organization_id = EXCLUDED.organization_id,
	requested_by = EXCLUDED.requested_by,
	requested_at = GREATEST(
		EXCLUDED.requested_at,
		workspace_admission_keepalive_intents.requested_at + INTERVAL '1 microsecond'
	),
	for_seconds = EXCLUDED.for_seconds,
	acked_at = NULL,
	acked_until = NULL,
	acked_result = NULL,
	updated_at = GREATEST(
		EXCLUDED.updated_at,
		workspace_admission_keepalive_intents.updated_at + INTERVAL '1 microsecond'
	)
RETURNING
	*;

-- name: GetWorkspaceAdmissionKeepaliveIntents :many
-- Lists keepalive intents, oldest request first. pending_only limits the list
-- to intents whose latest request was not acknowledged.
SELECT
	*
FROM
	workspace_admission_keepalive_intents
WHERE
	NOT @pending_only::boolean
	OR acked_at IS NULL
ORDER BY
	requested_at, workspace_id
LIMIT
	@limit_count::int;

-- name: AckWorkspaceAdmissionKeepaliveIntent :execrows
-- Acknowledges the latest keepalive request of a workspace. It only matches
-- when requested_at is the value the controller saw. Repeating an
-- acknowledgement keeps the first acknowledgement time.
UPDATE
	workspace_admission_keepalive_intents
SET
	acked_at = COALESCE(acked_at, @now::timestamptz),
	acked_until = sqlc.narg('until')::timestamptz,
	acked_result = @result::text,
	updated_at = CASE
		WHEN acked_at IS NULL THEN GREATEST(@now::timestamptz, updated_at + INTERVAL '1 microsecond')
		ELSE updated_at
	END
WHERE
	workspace_id = @workspace_id
	AND requested_at = @requested_at::timestamptz;
