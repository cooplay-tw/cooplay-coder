-- name: InsertWorkspaceAdmissionCreateOperation :one
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
) VALUES (
	@operation_id,
	@workspace_id,
	@workspace_build_id,
	@job_id,
	@organization_id,
	@owner_id,
	@grant_id,
	@run_id,
	@source,
	@created_at
)
RETURNING
	*;

-- name: GetWorkspaceAdmissionCreateOperationByOperationID :one
SELECT
	*
FROM
	workspace_admission_create_operations
WHERE
	operation_id = @operation_id;

-- name: GetWorkspaceAdmissionCreateOperationByWorkspaceID :one
SELECT
	*
FROM
	workspace_admission_create_operations
WHERE
	workspace_id = @workspace_id;

-- name: CountLiveWorkspaceAdmissionCreateOperationsByGrantID :one
-- Counts the workspaces created under a grant that are not deleted,
-- including workspaces whose first build has not run yet. A workspace
-- counts until its DELETE build succeeded.
SELECT
	COUNT(*)::integer
FROM
	workspace_admission_create_operations co
JOIN
	workspaces w ON w.id = co.workspace_id
WHERE
	co.grant_id = @grant_id::text
	AND NOT w.deleted;

-- name: InsertWorkspaceAdmissionDeleteOperation :one
INSERT INTO workspace_admission_delete_operations (
	operation_id,
	workspace_id,
	workspace_build_id,
	job_id,
	organization_id,
	grant_id,
	canceled_job_id,
	created_at
) VALUES (
	@operation_id,
	@workspace_id,
	@workspace_build_id,
	@job_id,
	@organization_id,
	@grant_id,
	@canceled_job_id,
	@created_at
)
RETURNING
	*;

-- name: GetWorkspaceAdmissionDeleteOperationByOperationID :one
SELECT
	*
FROM
	workspace_admission_delete_operations
WHERE
	operation_id = @operation_id;

-- name: UpsertWorkspaceAdmissionGrant :one
-- Mirrors the scope of a controller grant. The owner and organization of a
-- grant never change, so a mirror that names others returns no row. A
-- revocation is final: a later mirror never clears it.
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
) VALUES (
	@id,
	@owner_id,
	@organization_id,
	@template_ids,
	@name_prefix,
	@max_workspaces,
	@allow_delete,
	@expires_at,
	@revoked_at,
	@now,
	@now
)
ON CONFLICT (id) DO UPDATE SET
	template_ids = EXCLUDED.template_ids,
	name_prefix = EXCLUDED.name_prefix,
	max_workspaces = EXCLUDED.max_workspaces,
	allow_delete = EXCLUDED.allow_delete,
	expires_at = EXCLUDED.expires_at,
	revoked_at = COALESCE(workspace_admission_grants.revoked_at, EXCLUDED.revoked_at),
	updated_at = EXCLUDED.updated_at
WHERE
	workspace_admission_grants.owner_id = EXCLUDED.owner_id
	AND workspace_admission_grants.organization_id = EXCLUDED.organization_id
RETURNING
	*;

-- name: RevokeWorkspaceAdmissionGrant :one
-- Revokes a mirrored grant. Revoking it again keeps the first revocation.
UPDATE
	workspace_admission_grants
SET
	revoked_at = COALESCE(revoked_at, @now::timestamptz),
	updated_at = CASE WHEN revoked_at IS NULL THEN @now::timestamptz ELSE updated_at END
WHERE
	id = @id
RETURNING
	*;

-- name: GetWorkspaceAdmissionGrantByIDWithLock :one
-- Locks a mirrored grant for the rest of the transaction, so creates and
-- deletes under the same grant see each other's committed changes.
SELECT
	*
FROM
	workspace_admission_grants
WHERE
	id = @id
FOR UPDATE;
