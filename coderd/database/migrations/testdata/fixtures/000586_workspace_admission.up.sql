INSERT INTO workspace_admission_templates (
	template_id,
	organization_id,
	profile_parameter,
	created_at,
	updated_at
)
SELECT
	id,
	organization_id,
	'profile',
	NOW(),
	NOW()
FROM
	templates
ORDER BY
	created_at, id
LIMIT 1
ON CONFLICT DO NOTHING;

INSERT INTO workspace_admission_entitlements (
	user_id,
	template_id,
	organization_id,
	allowed_profiles,
	updated_at
)
SELECT
	u.id,
	wat.template_id,
	wat.organization_id,
	ARRAY['small', 'large']::text[],
	NOW()
FROM
	workspace_admission_templates wat
CROSS JOIN LATERAL (
	SELECT id FROM users ORDER BY created_at, id LIMIT 1
) u
ON CONFLICT DO NOTHING;

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
)
SELECT
	wb.job_id,
	wb.id,
	w.id,
	w.organization_id,
	w.owner_id,
	w.template_id,
	wb.template_version_id,
	'small',
	'waiting'::workspace_admission_status,
	'waiting_for_capacity',
	NOW(),
	NOW()
FROM
	workspace_builds wb
JOIN
	workspaces w ON w.id = wb.workspace_id
ORDER BY
	wb.created_at, wb.id
LIMIT 1
ON CONFLICT DO NOTHING;

INSERT INTO workspace_admission_stop_operations (
	operation_id,
	workspace_id,
	workspace_build_id,
	job_id,
	organization_id,
	reason,
	created_at
)
SELECT
	'5860a1c2-6d1e-4c39-9b8e-2f4f8f0e7a11'::uuid,
	w.id,
	wb.id,
	wb.job_id,
	w.organization_id,
	'fixture',
	NOW()
FROM
	workspace_builds wb
JOIN
	workspaces w ON w.id = wb.workspace_id
ORDER BY
	wb.created_at DESC, wb.id
LIMIT 1
ON CONFLICT DO NOTHING;
