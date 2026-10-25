-- Acquires the lock for a single job that isn't started, completed,
-- canceled, and that matches an array of provisioner types.
--
-- SKIP LOCKED is used to jump over locked rows. This prevents
-- multiple provisioners from acquiring the same jobs. See:
-- https://www.postgresql.org/docs/9.5/sql-select.html#SQL-FOR-UPDATE-SHARE
--
-- A managed START is a workspace START build whose workspace template is
-- listed in workspace_admission_templates. A managed START is only eligible
-- while a granted, unexpired admission that matches the build exists. A
-- missing, waiting, expired, mismatched or ended admission makes the job
-- ineligible. Acquiring a managed START marks its admission as acquired in
-- the same statement. If the admission changed after the candidate was
-- selected (for example a concurrent revoke), the admission update matches no
-- row and the job is not acquired.
--
-- STOP and DELETE builds release capacity, so they are acquired before other
-- jobs. Human-initiated jobs are then prioritized over prebuilds.
-- name: AcquireProvisionerJob :one
WITH candidate AS (
	SELECT
		potential_job.id,
		COALESCE(
			wb.transition = 'start'::workspace_transition AND wat.template_id IS NOT NULL,
			false
		) AS managed_start
	FROM
		provisioner_jobs AS potential_job
	LEFT JOIN
		workspace_builds wb ON wb.job_id = potential_job.id
	LEFT JOIN
		workspaces w ON w.id = wb.workspace_id
	LEFT JOIN
		workspace_admission_templates wat ON wat.template_id = w.template_id
	WHERE
		potential_job.started_at IS NULL
		AND potential_job.completed_at IS NULL
		AND potential_job.organization_id = @organization_id
		-- Ensure the caller has the correct provisioner.
		AND potential_job.provisioner = ANY(@types :: provisioner_type [ ])
		-- elsewhere, we use the tagset type, but here we use jsonb for backward compatibility
		-- they are aliases and the code that calls this query already relies on a different type
		AND provisioner_tagset_contains(@provisioner_tags :: jsonb, potential_job.tags :: jsonb)
		AND (
			NOT COALESCE(
				wb.transition = 'start'::workspace_transition AND wat.template_id IS NOT NULL,
				false
			)
			OR EXISTS (
				SELECT
					1
				FROM
					workspace_build_admissions wba
				WHERE
					wba.job_id = potential_job.id
					AND wba.status = 'granted'::workspace_admission_status
					AND wba.grant_expires_at > NOW()
					AND wba.workspace_build_id = wb.id
					AND wba.workspace_id = wb.workspace_id
					AND wba.owner_id = w.owner_id
					AND wba.template_id = w.template_id
					AND wba.template_version_id = wb.template_version_id
					AND wba.generation IS NOT NULL
					AND wba.reservation_id IS NOT NULL
					AND wba.operation_id IS NOT NULL
					AND wba.profile = (
						SELECT
							wbp.value
						FROM
							workspace_build_parameters wbp
						WHERE
							wbp.workspace_build_id = wb.id
							AND wbp.name = wat.profile_parameter
					)
			)
		)
	ORDER BY
		-- STOP and DELETE builds release capacity, so acquire them first.
		COALESCE(wb.transition IN ('stop'::workspace_transition, 'delete'::workspace_transition), false) DESC,
		-- Ensure that human-initiated jobs are prioritized over prebuilds.
		potential_job.initiator_id = 'c42fdf75-3097-471c-8c33-fb52454d81c0'::uuid ASC,
		potential_job.created_at ASC
	FOR UPDATE OF potential_job
	SKIP LOCKED
	LIMIT
		1
),
acquired_admission AS (
	UPDATE
		workspace_build_admissions
	SET
		status = 'acquired'::workspace_admission_status,
		acquired_at = @started_at,
		updated_at = @started_at
	WHERE
		job_id = (SELECT id FROM candidate WHERE managed_start)
		AND status = 'granted'::workspace_admission_status
		AND grant_expires_at > NOW()
	RETURNING
		job_id
)
UPDATE
	provisioner_jobs
SET
	started_at = @started_at,
	updated_at = @started_at,
	worker_id = @worker_id
WHERE
	id = (SELECT id FROM candidate)
	AND (
		NOT (SELECT managed_start FROM candidate)
		OR EXISTS (SELECT 1 FROM acquired_admission)
	)
RETURNING *;

-- name: GetProvisionerJobByID :one
SELECT
	*
FROM
	provisioner_jobs
WHERE
	id = $1;

-- name: GetProvisionerJobByIDForUpdate :one
-- Gets a single provisioner job by ID for update.
-- This is used to securely reap jobs that have been hung/pending for a long time.
SELECT
	*
FROM
	provisioner_jobs
WHERE
	id = $1
FOR UPDATE
SKIP LOCKED;

-- name: GetProvisionerJobByIDWithLock :one
-- Gets a provisioner job by ID with exclusive lock.
-- Blocks until the row is available for update.
SELECT
	*
FROM
	provisioner_jobs
WHERE
	id = $1
FOR UPDATE;

-- name: GetProvisionerJobsByIDsWithQueuePosition :many
WITH filtered_provisioner_jobs AS (
	-- Step 1: Filter provisioner_jobs
	SELECT
		id, created_at, tags
	FROM
		provisioner_jobs
	WHERE
		id = ANY(@ids :: uuid [ ]) -- Apply filter early to reduce dataset size before expensive JOIN
),
pending_jobs AS (
	-- Step 2: Extract only pending jobs that compete for provisioners.
	-- Managed START builds without a current grant wait for capacity, not
	-- for a provisioner, so they never take a provisioner queue position.
	SELECT
		pj.id, pj.initiator_id, pj.created_at, pj.tags,
		COALESCE(wb.transition IN ('stop'::workspace_transition, 'delete'::workspace_transition), false) AS releases_capacity
	FROM
		provisioner_jobs pj
	LEFT JOIN
		workspace_builds wb ON wb.job_id = pj.id
	WHERE
		pj.job_status = 'pending'
		AND NOT EXISTS (
			SELECT
				1
			FROM
				workspaces w
			JOIN
				workspace_admission_templates wat ON wat.template_id = w.template_id
			WHERE
				w.id = wb.workspace_id
				AND wb.transition = 'start'::workspace_transition
				AND NOT EXISTS (
					SELECT
						1
					FROM
						workspace_build_admissions wba
					WHERE
						wba.job_id = pj.id
						AND wba.status = 'granted'::workspace_admission_status
						AND wba.grant_expires_at > NOW()
				)
		)
),
unique_daemon_tags AS (
	SELECT DISTINCT tags FROM provisioner_daemons pd
	WHERE pd.last_seen_at IS NOT NULL
	  AND pd.last_seen_at >= (NOW() - (@stale_interval_ms::bigint || ' ms')::interval)
),
relevant_daemon_tags AS (
	SELECT udt.tags
	FROM unique_daemon_tags udt
	WHERE EXISTS (
		SELECT 1 FROM filtered_provisioner_jobs fpj
		WHERE provisioner_tagset_contains(udt.tags, fpj.tags)
	)
),
ranked_jobs AS (
	-- Step 3: Rank only pending jobs based on provisioner availability
	SELECT
		pj.id,
		pj.created_at,
		-- Keep in sync with the ordering of AcquireProvisionerJob.
		ROW_NUMBER() OVER (PARTITION BY rdt.tags ORDER BY pj.releases_capacity DESC, pj.initiator_id = 'c42fdf75-3097-471c-8c33-fb52454d81c0'::uuid ASC, pj.created_at ASC) AS queue_position,
		COUNT(*) OVER (PARTITION BY rdt.tags) AS queue_size
	FROM
		pending_jobs pj
	INNER JOIN
		relevant_daemon_tags rdt
	ON
		provisioner_tagset_contains(rdt.tags, pj.tags)
),
final_jobs AS (
	-- Step 4: Compute best queue position and max queue size per job
	SELECT
		fpj.id,
		fpj.created_at,
		COALESCE(MIN(rj.queue_position), 0) :: BIGINT AS queue_position, -- Best queue position across provisioners
		COALESCE(MAX(rj.queue_size), 0) :: BIGINT AS queue_size -- Max queue size across provisioners
	FROM
		filtered_provisioner_jobs fpj -- Use the pre-filtered dataset instead of full provisioner_jobs
			LEFT JOIN ranked_jobs rj
					ON fpj.id = rj.id -- Join with the ranking jobs CTE to assign a rank to each specified provisioner job.
	GROUP BY
		fpj.id, fpj.created_at
)
SELECT
	-- Step 5: Final SELECT with INNER JOIN provisioner_jobs
	fj.id,
	fj.created_at,
	sqlc.embed(pj),
	fj.queue_position,
	fj.queue_size
FROM
	final_jobs fj
		INNER JOIN provisioner_jobs pj
				ON fj.id = pj.id -- Ensure we retrieve full details from `provisioner_jobs`.
                                 -- JOIN with pj is required for sqlc.embed(pj) to compile successfully.
ORDER BY
	fj.created_at;

-- name: GetProvisionerJobsByOrganizationAndStatusWithQueuePositionAndProvisioner :many
WITH pending_jobs AS (
    SELECT
        pending.id, pending.initiator_id, pending.created_at,
        COALESCE(pending_wb.transition IN ('stop'::workspace_transition, 'delete'::workspace_transition), false) AS releases_capacity
    FROM
        provisioner_jobs pending
    LEFT JOIN
        workspace_builds pending_wb ON pending_wb.job_id = pending.id
    WHERE
        pending.started_at IS NULL
    AND
        pending.canceled_at IS NULL
    AND
        pending.completed_at IS NULL
    AND
        pending.error IS NULL
    -- Managed START builds without a current grant wait for capacity, not
    -- for a provisioner, so they never take a provisioner queue position.
    AND NOT EXISTS (
        SELECT
            1
        FROM
            workspaces pending_w
        JOIN
            workspace_admission_templates pending_wat ON pending_wat.template_id = pending_w.template_id
        WHERE
            pending_w.id = pending_wb.workspace_id
            AND pending_wb.transition = 'start'::workspace_transition
            AND NOT EXISTS (
                SELECT
                    1
                FROM
                    workspace_build_admissions pending_wba
                WHERE
                    pending_wba.job_id = pending.id
                    AND pending_wba.status = 'granted'::workspace_admission_status
                    AND pending_wba.grant_expires_at > NOW()
            )
    )
),
queue_position AS (
    SELECT
        id,
        -- Keep in sync with the ordering of AcquireProvisionerJob.
        ROW_NUMBER() OVER (ORDER BY releases_capacity DESC, initiator_id = 'c42fdf75-3097-471c-8c33-fb52454d81c0'::uuid ASC, created_at ASC) AS queue_position
    FROM
        pending_jobs
),
queue_size AS (
	SELECT COUNT(*) AS count FROM pending_jobs
)
SELECT
	sqlc.embed(pj),
    COALESCE(qp.queue_position, 0) AS queue_position,
    COALESCE(qs.count, 0) AS queue_size,
	-- Use subquery to utilize ORDER BY in array_agg since it cannot be
	-- combined with FILTER.
	(
		SELECT
			-- Order for stable output.
			array_agg(pd.id ORDER BY pd.created_at ASC)::uuid[]
		FROM
			provisioner_daemons pd
		WHERE
			-- See AcquireProvisionerJob.
			pj.started_at IS NULL
			AND pj.organization_id = pd.organization_id
			AND pj.provisioner = ANY(pd.provisioners)
			AND provisioner_tagset_contains(pd.tags, pj.tags)
	) AS available_workers,
	-- Include template and workspace information.
	COALESCE(tv.name, '') AS template_version_name,
	t.id AS template_id,
	COALESCE(t.name, '') AS template_name,
	COALESCE(t.display_name, '') AS template_display_name,
	COALESCE(t.icon, '') AS template_icon,
	w.id AS workspace_id,
	COALESCE(w.name, '') AS workspace_name,
	-- Include the name of the provisioner_daemon associated to the job
	COALESCE(pd.name, '') AS worker_name,
	wb.transition as workspace_build_transition
FROM
	provisioner_jobs pj
LEFT JOIN
	queue_position qp ON qp.id = pj.id
LEFT JOIN
	queue_size qs ON TRUE
LEFT JOIN
	workspace_builds wb ON wb.id = CASE WHEN pj.input ? 'workspace_build_id' THEN (pj.input->>'workspace_build_id')::uuid END
LEFT JOIN
	workspaces w ON (
		w.id = wb.workspace_id
		AND w.organization_id = pj.organization_id
	)
LEFT JOIN
	-- We should always have a template version, either explicitly or implicitly via workspace build.
	template_versions tv ON (
		tv.id = CASE WHEN pj.input ? 'template_version_id' THEN (pj.input->>'template_version_id')::uuid ELSE wb.template_version_id END
		AND tv.organization_id = pj.organization_id
	)
LEFT JOIN
	templates t ON (
		t.id = tv.template_id
		AND t.organization_id = pj.organization_id
	)
LEFT JOIN
	-- Join to get the daemon name corresponding to the job's worker_id
	provisioner_daemons pd ON pd.id = pj.worker_id
WHERE
	pj.organization_id = @organization_id::uuid
	AND (COALESCE(array_length(@ids::uuid[], 1), 0) = 0 OR pj.id = ANY(@ids::uuid[]))
	AND (COALESCE(array_length(@status::provisioner_job_status[], 1), 0) = 0 OR pj.job_status = ANY(@status::provisioner_job_status[]))
	AND (@tags::tagset = 'null'::tagset OR provisioner_tagset_contains(pj.tags::tagset, @tags::tagset))
	AND (@initiator_id::uuid = '00000000-0000-0000-0000-000000000000'::uuid OR pj.initiator_id = @initiator_id::uuid)
GROUP BY
	pj.id,
	qp.queue_position,
	qs.count,
	tv.name,
	t.id,
	t.name,
	t.display_name,
	t.icon,
	w.id,
	w.name,
	pd.name,
	wb.transition
ORDER BY
	pj.created_at DESC
LIMIT
	sqlc.narg('limit')::int;

-- name: GetProvisionerJobsCreatedAfter :many
SELECT * FROM provisioner_jobs WHERE created_at > $1;

-- name: InsertProvisionerJob :one
INSERT INTO
	provisioner_jobs (
		id,
		created_at,
		updated_at,
		organization_id,
		initiator_id,
		provisioner,
		storage_method,
		file_id,
		"type",
		"input",
		tags,
		trace_metadata,
		logs_overflowed
	)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING *;

-- name: UpdateProvisionerJobByID :exec
UPDATE
	provisioner_jobs
SET
	updated_at = $2
WHERE
	id = $1;

-- name: UpdateProvisionerJobWithCancelByID :exec
UPDATE
	provisioner_jobs
SET
	canceled_at = $2,
	completed_at = $3
WHERE
	id = $1;

-- name: UpdateProvisionerJobWithCompleteByID :exec
UPDATE
	provisioner_jobs
SET
	updated_at = $2,
	completed_at = $3,
	error = $4,
	error_code = $5
WHERE
	id = $1;

-- name: UpdateProvisionerJobWithCompleteWithStartedAtByID :exec
UPDATE
	provisioner_jobs
SET
	updated_at = $2,
	completed_at = $3,
	error = $4,
	error_code = $5,
	started_at = $6
WHERE
	id = $1;

-- name: GetProvisionerJobsToBeReaped :many
-- A pending managed START whose admission is waiting is a valid capacity wait
-- and is never reaped. A granted managed START measures its pending time from
-- the later of the last job update and the grant, so a job that waited for
-- capacity is not reaped right after it is granted. Keep in sync with the
-- post-lock recheck in coderd/jobreaper.
SELECT
	pj.*
FROM
	provisioner_jobs pj
LEFT JOIN (
	SELECT
		wb.job_id,
		wba.status AS admission_status,
		wba.granted_at AS admission_granted_at
	FROM
		workspace_builds wb
	JOIN
		workspaces w ON w.id = wb.workspace_id
	JOIN
		workspace_admission_templates wat ON wat.template_id = w.template_id
	LEFT JOIN
		workspace_build_admissions wba ON wba.job_id = wb.job_id
	WHERE
		wb.transition = 'start'::workspace_transition
) managed_start ON managed_start.job_id = pj.id
WHERE
	(
		-- If the job has not been started before @pending_since, reap it.
		pj.started_at IS NULL
		AND pj.completed_at IS NULL
		AND managed_start.admission_status IS DISTINCT FROM 'waiting'::workspace_admission_status
		AND CASE
			WHEN managed_start.admission_status = 'granted'::workspace_admission_status
				THEN GREATEST(pj.updated_at, managed_start.admission_granted_at)
			ELSE pj.updated_at
		END < @pending_since::timestamptz
	)
	OR
	(
		-- If the job has been started but not completed before @hung_since, reap it.
		pj.updated_at < @hung_since::timestamptz
		AND pj.started_at IS NOT NULL
		AND pj.completed_at IS NULL
	)
-- To avoid repeatedly attempting to reap the same jobs, we randomly order and limit to @max_jobs.
ORDER BY random()
LIMIT @max_jobs;

-- name: InsertProvisionerJobTimings :many
INSERT INTO provisioner_job_timings (job_id, started_at, ended_at, stage, source, action, resource)
SELECT
    @job_id::uuid AS provisioner_job_id,
    unnest(@started_at::timestamptz[]),
    unnest(@ended_at::timestamptz[]),
    unnest(@stage::provisioner_job_timing_stage[]),
    unnest(@source::text[]),
    unnest(@action::text[]),
    unnest(@resource::text[])
RETURNING *;

-- name: GetProvisionerJobTimingsByJobID :many
SELECT * FROM provisioner_job_timings
WHERE job_id = $1
ORDER BY started_at ASC;
