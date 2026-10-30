-- Power management of managed workspaces. The external platform controller
-- suspends and restores the microVMs of managed workspaces while their Coder
-- build stays running. Coder records client intents (wake, keepalive) for the
-- controller to act on, mirrors the controller's power state for display,
-- keeps idempotency records of the workspaces the controller creates and
-- deletes on behalf of their owners, and mirrors the scope of the capability
-- grants those creates run under.

-- Wake intents: a client tried to reach an agent of a managed workspace that
-- may be asleep. One row per workspace; repeated requests update it. The
-- controller lists pending rows, wakes the workspace and acknowledges the
-- request it saw. A request recorded after the acknowledgement makes the row
-- pending again.
CREATE TABLE workspace_admission_wake_intents (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id UUID NOT NULL,
    source TEXT NOT NULL,
    requested_by UUID REFERENCES users(id) ON DELETE SET NULL,
    first_requested_at TIMESTAMPTZ NOT NULL,
    last_requested_at TIMESTAMPTZ NOT NULL,
    request_count INTEGER NOT NULL,
    acked_at TIMESTAMPTZ,
    acked_power_epoch BIGINT,
    acked_result TEXT,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT workspace_admission_wake_intents_source_check CHECK (
        source IN ('tailnet', 'cli', 'pty', 'app')
    ),
    CONSTRAINT workspace_admission_wake_intents_request_count_check CHECK (
        request_count >= 1
    ),
    CONSTRAINT workspace_admission_wake_intents_requested_at_check CHECK (
        last_requested_at >= first_requested_at
    ),
    CONSTRAINT workspace_admission_wake_intents_acked_result_check CHECK (
        acked_result IS NULL OR acked_result IN ('woken', 'already_running', 'failed', 'ignored')
    ),
    -- An acknowledgement always records its time, power epoch and result.
    CONSTRAINT workspace_admission_wake_intents_ack_fields_check CHECK (
        (acked_at IS NULL) = (acked_power_epoch IS NULL)
        AND (acked_at IS NULL) = (acked_result IS NULL)
    )
);

-- Serves the change cursor of the admission service long poll.
CREATE INDEX workspace_admission_wake_intents_updated_at_idx
    ON workspace_admission_wake_intents (updated_at);

CREATE INDEX workspace_admission_wake_intents_pending_idx
    ON workspace_admission_wake_intents (first_requested_at, workspace_id)
    WHERE acked_at IS NULL;

COMMENT ON TABLE workspace_admission_wake_intents IS
    'Requests to wake a managed workspace, recorded when a client connects to one of its agents. A row is pending until the controller acknowledges its latest request.';

COMMENT ON COLUMN workspace_admission_wake_intents.agent_id IS
    'Agent the latest request tried to reach. Not a foreign key: the intent outlives agent rows of replaced builds.';

COMMENT ON COLUMN workspace_admission_wake_intents.source IS
    'Client path of the latest request: tailnet (coordinated tunnel), cli (explicit wake), pty (web terminal) or app (workspace app proxy).';

COMMENT ON COLUMN workspace_admission_wake_intents.first_requested_at IS
    'First request since the last acknowledgement.';

COMMENT ON COLUMN workspace_admission_wake_intents.last_requested_at IS
    'Latest request. Strictly increases with every recorded request, so an acknowledgement that names it covers exactly the requests the controller saw.';

COMMENT ON COLUMN workspace_admission_wake_intents.request_count IS
    'Requests since the last acknowledgement, including coalesced repeats.';

COMMENT ON COLUMN workspace_admission_wake_intents.acked_power_epoch IS
    'Controller power epoch that the acknowledgement reported.';

-- Keepalive intents: a user asked to keep a managed workspace awake, for
-- example from the sleep notice on the workspace page. One row per workspace;
-- a newer request replaces the previous one.
CREATE TABLE workspace_admission_keepalive_intents (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    requested_by UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    requested_at TIMESTAMPTZ NOT NULL,
    for_seconds INTEGER NOT NULL,
    acked_at TIMESTAMPTZ,
    acked_until TIMESTAMPTZ,
    acked_result TEXT,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT workspace_admission_keepalive_intents_for_seconds_check CHECK (
        for_seconds > 0 AND for_seconds <= 43200
    ),
    CONSTRAINT workspace_admission_keepalive_intents_acked_result_check CHECK (
        acked_result IS NULL OR acked_result IN ('applied', 'denied')
    ),
    CONSTRAINT workspace_admission_keepalive_intents_ack_fields_check CHECK (
        (acked_at IS NULL) = (acked_result IS NULL)
        AND (acked_until IS NULL OR acked_at IS NOT NULL)
    )
);

CREATE INDEX workspace_admission_keepalive_intents_updated_at_idx
    ON workspace_admission_keepalive_intents (updated_at);

CREATE INDEX workspace_admission_keepalive_intents_pending_idx
    ON workspace_admission_keepalive_intents (requested_at, workspace_id)
    WHERE acked_at IS NULL;

COMMENT ON TABLE workspace_admission_keepalive_intents IS
    'Requests to keep a managed workspace awake. A row is pending until the controller acknowledges its latest request.';

COMMENT ON COLUMN workspace_admission_keepalive_intents.requested_at IS
    'Latest request. Strictly increases with every recorded request, so an acknowledgement that names it covers exactly the request the controller saw.';

COMMENT ON COLUMN workspace_admission_keepalive_intents.for_seconds IS
    'Requested keepalive duration. The controller caps it at its own maximum.';

COMMENT ON COLUMN workspace_admission_keepalive_intents.acked_until IS
    'Keepalive end the controller applied, if any.';

-- Workspaces the controller created on behalf of their owners, keyed by the
-- controller operation so retries return the original workspace. Also the
-- source of the "created by Run under grant" display.
CREATE TABLE workspace_admission_create_operations (
    operation_id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL UNIQUE REFERENCES workspaces(id) ON DELETE CASCADE,
    workspace_build_id UUID NOT NULL UNIQUE REFERENCES workspace_builds(id) ON DELETE CASCADE,
    job_id UUID NOT NULL REFERENCES provisioner_jobs(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    grant_id TEXT,
    run_id TEXT,
    source JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT workspace_admission_create_operations_grant_id_check CHECK (
        grant_id IS NULL OR grant_id ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
    ),
    CONSTRAINT workspace_admission_create_operations_run_id_check CHECK (
        run_id IS NULL OR run_id ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
    ),
    CONSTRAINT workspace_admission_create_operations_source_check CHECK (
        CASE
            WHEN jsonb_typeof(source) = 'object' THEN
                COALESCE(source->>'kind', '') IN ('employee', 'agent_grant')
            ELSE false
        END
    ),
    CONSTRAINT workspace_admission_create_operations_grant_check CHECK (
        source->>'kind' IS DISTINCT FROM 'agent_grant' OR grant_id IS NOT NULL
    )
);

CREATE INDEX workspace_admission_create_operations_grant_id_idx
    ON workspace_admission_create_operations (grant_id)
    WHERE grant_id IS NOT NULL;

COMMENT ON TABLE workspace_admission_create_operations IS
    'Workspaces created through the workspace admission service on behalf of their owners, keyed by the controller operation ID for idempotent retries.';

COMMENT ON COLUMN workspace_admission_create_operations.grant_id IS
    'Controller capability grant the workspace was created under, if any. Grant IDs are opaque controller identifiers (for example g-<ulid>), not secrets.';

COMMENT ON COLUMN workspace_admission_create_operations.run_id IS
    'Controller Run that created the workspace, if any. Run IDs are opaque controller identifiers (for example r-<ulid>).';

COMMENT ON COLUMN workspace_admission_create_operations.source IS
    'Create source as sent by the controller: kind (employee or agent_grant), grant_id, run_id, harness, initiator_workspace_id and clone_of_workspace_id.';

-- DELETE builds that the admission controller requested, keyed by the
-- controller operation so retries return the original build.
CREATE TABLE workspace_admission_delete_operations (
    operation_id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    workspace_build_id UUID NOT NULL UNIQUE REFERENCES workspace_builds(id) ON DELETE CASCADE,
    job_id UUID NOT NULL REFERENCES provisioner_jobs(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    grant_id TEXT,
    canceled_job_id UUID,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT workspace_admission_delete_operations_grant_id_check CHECK (
        grant_id IS NULL OR grant_id ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
    )
);

CREATE INDEX workspace_admission_delete_operations_workspace_id_idx
    ON workspace_admission_delete_operations (workspace_id);

COMMENT ON TABLE workspace_admission_delete_operations IS
    'DELETE builds created through the workspace admission service, keyed by the controller operation ID for idempotent retries.';

COMMENT ON COLUMN workspace_admission_delete_operations.canceled_job_id IS
    'Pending START job that was canceled before it was acquired so the DELETE build could be created.';

-- Power state of managed workspaces as pushed by the controller. A missing
-- row means the controller has not reported the workspace yet; a row with
-- off_at set means the controller reports it as off.
CREATE TABLE workspace_admission_workspace_states (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    power_state TEXT NOT NULL,
    power_epoch BIGINT NOT NULL,
    generation BIGINT NOT NULL,
    since TIMESTAMPTZ NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    sleep_notice_at TIMESTAMPTZ,
    keepalive_until TIMESTAMPTZ,
    runs JSONB NOT NULL DEFAULT '[]'::jsonb,
    last_job JSONB,
    git JSONB,
    pr JSONB,
    last_sample JSONB,
    off_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT workspace_admission_workspace_states_power_state_check CHECK (
        power_state IN ('running', 'reduced', 'paused', 'hibernated', 'cold', 'waking', 'sleeping', 'unknown')
    ),
    CONSTRAINT workspace_admission_workspace_states_power_epoch_check CHECK (
        power_epoch >= 0
    ),
    CONSTRAINT workspace_admission_workspace_states_generation_check CHECK (
        generation >= 0
    ),
    -- At most 20 runs, each started by an employee or under a grant and in a
    -- known run state.
    CONSTRAINT workspace_admission_workspace_states_runs_check CHECK (
        CASE
            WHEN jsonb_typeof(runs) = 'array' THEN
                jsonb_array_length(runs) <= 20
                AND NOT jsonb_path_exists(runs, '$[*] ? (!(@.started_by == "employee" || @.started_by == "grant"))')
                AND NOT jsonb_path_exists(runs, '$[*] ? (!(@.state == "running" || @.state == "waiting_model" || @.state == "waiting_human" || @.state == "idle" || @.state == "done" || @.state == "failed"))')
            ELSE false
        END
    ),
    CONSTRAINT workspace_admission_workspace_states_facts_check CHECK (
        (last_job IS NULL OR jsonb_typeof(last_job) = 'object')
        AND (git IS NULL OR jsonb_typeof(git) = 'object')
        AND (pr IS NULL OR jsonb_typeof(pr) = 'object')
    ),
    CONSTRAINT workspace_admission_workspace_states_last_sample_check CHECK (
        last_sample IS NULL OR octet_length(last_sample::text) <= 4096
    )
);

CREATE INDEX workspace_admission_workspace_states_updated_at_idx
    ON workspace_admission_workspace_states (updated_at);

COMMENT ON TABLE workspace_admission_workspace_states IS
    'Power state, runs and facts of managed workspaces as pushed by the controller, for display. The controller is authoritative; a missing row means not reported, a row with off_at means off.';

COMMENT ON COLUMN workspace_admission_workspace_states.power_epoch IS
    'Controller power epoch of the state. Updates with a lower epoch are rejected; an equal epoch may update runs, job, facts and notices.';

COMMENT ON COLUMN workspace_admission_workspace_states.sleep_notice_at IS
    'Time of a pending policy sleep. The workspace page offers a keepalive while it is set.';

COMMENT ON COLUMN workspace_admission_workspace_states.off_at IS
    'Set when the controller reported the workspace as off. The row then only keeps power_epoch, so that a delayed older state is still rejected, and readers treat it as absent. A newer state clears it.';

-- Scope mirror of controller capability grants. The controller is
-- authoritative for grants; Coder only checks this mirror atomically with
-- workspace creation so that revocation fails closed. Grant secrets never
-- reach Coder.
CREATE TABLE workspace_admission_grants (
    id TEXT PRIMARY KEY,
    owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    template_ids UUID[] NOT NULL DEFAULT '{}',
    name_prefix TEXT NOT NULL DEFAULT '',
    max_workspaces INTEGER NOT NULL,
    allow_delete BOOLEAN NOT NULL DEFAULT false,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT workspace_admission_grants_id_check CHECK (
        id ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
    ),
    CONSTRAINT workspace_admission_grants_max_workspaces_check CHECK (
        max_workspaces >= 0
    )
);

CREATE INDEX workspace_admission_grants_owner_id_idx
    ON workspace_admission_grants (owner_id);

COMMENT ON TABLE workspace_admission_grants IS
    'Scope mirror of controller capability grants (IDs and limits only, never secrets), checked atomically when the controller creates a workspace under a grant.';

COMMENT ON COLUMN workspace_admission_grants.id IS
    'Controller grant ID, an opaque controller identifier (for example g-<ulid>).';

COMMENT ON COLUMN workspace_admission_grants.template_ids IS
    'Templates the grant may create workspaces from. Empty allows every template the owner is entitled to.';

COMMENT ON COLUMN workspace_admission_grants.name_prefix IS
    'Workspaces created under the grant must have names starting with this prefix. Empty allows any name.';

COMMENT ON COLUMN workspace_admission_grants.max_workspaces IS
    'Backstop limit of live workspaces created under the grant. The controller count is authoritative.';
