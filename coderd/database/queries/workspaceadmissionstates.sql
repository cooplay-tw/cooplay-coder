-- name: UpsertWorkspaceAdmissionWorkspaceState :one
-- Stores the power state, runs and facts the controller pushed for a managed
-- workspace, compare-and-set on power_epoch. A state with an older epoch
-- than the stored one, including the epoch kept while the controller
-- reports the workspace as off, changes nothing and returns no row. An
-- equal epoch replaces the state, for example when a wake finishes or the
-- runs change. updated_at strictly increases so the admission change cursor
-- moves with every accepted state.
INSERT INTO workspace_admission_workspace_states (
	workspace_id,
	organization_id,
	power_state,
	power_epoch,
	generation,
	since,
	reason,
	sleep_notice_at,
	keepalive_until,
	runs,
	last_job,
	git,
	pr,
	last_sample,
	off_at,
	updated_at
) VALUES (
	@workspace_id,
	@organization_id,
	@power_state,
	@power_epoch,
	@generation,
	@since,
	@reason,
	@sleep_notice_at,
	@keepalive_until,
	@runs,
	@last_job,
	@git,
	@pr,
	@last_sample,
	NULL,
	@updated_at
)
ON CONFLICT (workspace_id) DO UPDATE SET
	organization_id = EXCLUDED.organization_id,
	power_state = EXCLUDED.power_state,
	power_epoch = EXCLUDED.power_epoch,
	generation = EXCLUDED.generation,
	since = EXCLUDED.since,
	reason = EXCLUDED.reason,
	sleep_notice_at = EXCLUDED.sleep_notice_at,
	keepalive_until = EXCLUDED.keepalive_until,
	runs = EXCLUDED.runs,
	last_job = EXCLUDED.last_job,
	git = EXCLUDED.git,
	pr = EXCLUDED.pr,
	last_sample = EXCLUDED.last_sample,
	off_at = NULL,
	updated_at = GREATEST(
		EXCLUDED.updated_at,
		workspace_admission_workspace_states.updated_at + INTERVAL '1 microsecond'
	)
WHERE
	workspace_admission_workspace_states.power_epoch <= EXCLUDED.power_epoch
RETURNING
	*;

-- name: ClearWorkspaceAdmissionWorkspaceState :execrows
-- Records that the controller reports a managed workspace as off. The row
-- keeps its power_epoch so that a delayed older state is still rejected.
-- Clearing a state that is already off changes nothing.
UPDATE
	workspace_admission_workspace_states
SET
	off_at = @now::timestamptz,
	updated_at = GREATEST(@now::timestamptz, updated_at + INTERVAL '1 microsecond')
WHERE
	workspace_id = @workspace_id
	AND off_at IS NULL;

-- name: GetWorkspaceAdmissionWorkspaceStateByWorkspaceID :one
-- Returns the stored state of a managed workspace, also while the
-- controller reports it as off.
SELECT
	*
FROM
	workspace_admission_workspace_states
WHERE
	workspace_id = @workspace_id;

-- name: GetWorkspaceAdmissionCooplayByWorkspaceIDs :many
-- Returns what the workspace page shows about managed workspaces: the power
-- state, runs and facts the controller reported, which are absent while it
-- reports the workspace as off, and how the controller created the
-- workspace, with the scope mirror of the grant it was created under.
-- Workspaces with neither are omitted.
SELECT
	w.id AS workspace_id,
	wss.power_state,
	wss.since AS power_since,
	wss.reason AS power_reason,
	wss.sleep_notice_at,
	wss.keepalive_until,
	wss.runs,
	wss.last_job,
	wss.git,
	wss.pr,
	wco.source AS create_source,
	wag.expires_at AS grant_expires_at,
	wag.revoked_at AS grant_revoked_at
FROM
	workspaces w
LEFT JOIN
	workspace_admission_workspace_states wss ON wss.workspace_id = w.id AND wss.off_at IS NULL
LEFT JOIN
	workspace_admission_create_operations wco ON wco.workspace_id = w.id
LEFT JOIN
	workspace_admission_grants wag ON wag.id = wco.grant_id
WHERE
	w.id = ANY(@workspace_ids :: uuid[])
	AND (wss.workspace_id IS NOT NULL OR wco.workspace_id IS NOT NULL);
