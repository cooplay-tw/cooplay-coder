INSERT INTO workspace_admission_wake_intents (
	workspace_id,
	organization_id,
	agent_id,
	source,
	requested_by,
	first_requested_at,
	last_requested_at,
	request_count,
	acked_at,
	acked_power_epoch,
	acked_result,
	updated_at
)
SELECT
	w.id,
	w.organization_id,
	'58700000-0000-4000-8000-000000000001'::uuid,
	'tailnet',
	w.owner_id,
	NOW() - INTERVAL '2 seconds',
	NOW() - INTERVAL '1 second',
	2,
	NOW(),
	7,
	'woken',
	NOW()
FROM
	workspaces w
ORDER BY
	w.created_at, w.id
LIMIT 1
ON CONFLICT DO NOTHING;

INSERT INTO workspace_admission_keepalive_intents (
	workspace_id,
	organization_id,
	requested_by,
	requested_at,
	for_seconds,
	updated_at
)
SELECT
	w.id,
	w.organization_id,
	w.owner_id,
	NOW(),
	7200,
	NOW()
FROM
	workspaces w
ORDER BY
	w.created_at, w.id
LIMIT 1
ON CONFLICT DO NOTHING;

INSERT INTO workspace_admission_create_operations (
	operation_id,
	workspace_id,
	workspace_build_id,
	job_id,
	organization_id,
	owner_id,
	grant_id,
	run_id,
	source,
	created_at
)
SELECT
	'58700000-0000-4000-8000-000000000002'::uuid,
	w.id,
	wb.id,
	wb.job_id,
	w.organization_id,
	w.owner_id,
	'g-01jbx5870000000000000000gr',
	'r-01jbx5870000000000000000rn',
	'{"kind": "agent_grant", "grant_id": "g-01jbx5870000000000000000gr", "run_id": "r-01jbx5870000000000000000rn", "harness": "codex", "initiator_workspace_id": "", "clone_of_workspace_id": ""}'::jsonb,
	NOW()
FROM
	workspace_builds wb
JOIN
	workspaces w ON w.id = wb.workspace_id
ORDER BY
	wb.created_at, wb.id
LIMIT 1
ON CONFLICT DO NOTHING;

INSERT INTO workspace_admission_delete_operations (
	operation_id,
	workspace_id,
	workspace_build_id,
	job_id,
	organization_id,
	grant_id,
	created_at
)
SELECT
	'58700000-0000-4000-8000-000000000005'::uuid,
	w.id,
	wb.id,
	wb.job_id,
	w.organization_id,
	'g-01jbx5870000000000000000gr',
	NOW()
FROM
	workspace_builds wb
JOIN
	workspaces w ON w.id = wb.workspace_id
ORDER BY
	wb.created_at DESC, wb.id
LIMIT 1
ON CONFLICT DO NOTHING;

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
	updated_at
)
SELECT
	w.id,
	w.organization_id,
	'hibernated',
	7,
	1,
	NOW(),
	'idle',
	NULL,
	NOW() + INTERVAL '2 hours',
	'[{"id": "r-01jbx5870000000000000000rn", "harness": "codex", "mode": "A", "state": "waiting_human", "waiting_reason": "approval", "started_by": "grant", "grant_id": "g-01jbx5870000000000000000gr", "last_event_at": "2026-10-30T00:00:00Z"}]'::jsonb,
	'{"name": "test", "state": "done"}'::jsonb,
	'{"branch": "main"}'::jsonb,
	'{"number": 1}'::jsonb,
	'{"memory_mib": 512}'::jsonb,
	NOW()
FROM
	workspaces w
ORDER BY
	w.created_at, w.id
LIMIT 1
ON CONFLICT DO NOTHING;

INSERT INTO workspace_admission_grants (
	id,
	owner_id,
	organization_id,
	template_ids,
	name_prefix,
	max_workspaces,
	allow_delete,
	expires_at,
	revoked_at,
	created_at,
	updated_at
)
SELECT
	'g-01jbx5870000000000000000gr',
	w.owner_id,
	w.organization_id,
	ARRAY[w.template_id]::uuid[],
	'agent-',
	3,
	true,
	NOW() + INTERVAL '1 day',
	NULL,
	NOW(),
	NOW()
FROM
	workspaces w
ORDER BY
	w.created_at, w.id
LIMIT 1
ON CONFLICT DO NOTHING;
