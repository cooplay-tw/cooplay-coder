-- Workspace admission lets an external capacity controller decide when a
-- workspace START build on a managed template may reach a provisioner. Coder
-- commits the workspace, the pending build and a waiting admission row in one
-- transaction; provisioners only acquire a managed START after the controller
-- records a valid grant.
CREATE TYPE workspace_admission_status AS ENUM (
    'waiting',
    'granted',
    'acquired',
    'rejected',
    'canceled',
    'revoked'
);

COMMENT ON TYPE workspace_admission_status IS
    'Lifecycle of a workspace START admission request: waiting for capacity, granted by the controller, acquired by a provisioner, or ended by rejection, cancellation or revocation.';

CREATE TABLE workspace_admission_templates (
    template_id UUID PRIMARY KEY REFERENCES templates(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    profile_parameter TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT workspace_admission_templates_profile_parameter_check CHECK (profile_parameter <> '')
);

COMMENT ON TABLE workspace_admission_templates IS
    'Templates whose START builds require a workspace admission grant before a provisioner may acquire them.';

COMMENT ON COLUMN workspace_admission_templates.profile_parameter IS
    'Name of the build parameter whose value selects the resource profile of a START build.';

CREATE TABLE workspace_admission_entitlements (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    template_id UUID NOT NULL REFERENCES workspace_admission_templates(template_id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    allowed_profiles TEXT[] NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, template_id)
);

CREATE INDEX workspace_admission_entitlements_template_id_idx
    ON workspace_admission_entitlements (template_id);

COMMENT ON TABLE workspace_admission_entitlements IS
    'Profiles a user may request when starting workspaces of a managed template. Checked synchronously when the START build is created.';

CREATE TABLE workspace_build_admissions (
    job_id UUID PRIMARY KEY REFERENCES provisioner_jobs(id) ON DELETE CASCADE,
    workspace_build_id UUID NOT NULL UNIQUE REFERENCES workspace_builds(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    template_id UUID NOT NULL REFERENCES templates(id) ON DELETE CASCADE,
    template_version_id UUID NOT NULL REFERENCES template_versions(id) ON DELETE CASCADE,
    profile TEXT NOT NULL,
    status workspace_admission_status NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    queue_position INTEGER,
    operation_id UUID,
    reservation_id UUID,
    generation BIGINT,
    grant_expires_at TIMESTAMPTZ,
    queued_at TIMESTAMPTZ NOT NULL,
    granted_at TIMESTAMPTZ,
    acquired_at TIMESTAMPTZ,
    decided_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    -- A grant is only meaningful together with the controller reservation
    -- that backs it, so granted and acquired rows must carry it.
    CONSTRAINT workspace_build_admissions_grant_fields_check CHECK (
        status NOT IN ('granted', 'acquired') OR (
            reservation_id IS NOT NULL
            AND generation IS NOT NULL
            AND operation_id IS NOT NULL
            AND grant_expires_at IS NOT NULL
            AND granted_at IS NOT NULL
        )
    ),
    CONSTRAINT workspace_build_admissions_acquired_at_check CHECK (
        status <> 'acquired' OR acquired_at IS NOT NULL
    ),
    CONSTRAINT workspace_build_admissions_queue_position_check CHECK (
        queue_position IS NULL OR queue_position >= 0
    )
);

CREATE INDEX workspace_build_admissions_status_idx
    ON workspace_build_admissions (status, queued_at, job_id);

CREATE INDEX workspace_build_admissions_workspace_id_idx
    ON workspace_build_admissions (workspace_id);

-- Serves the change cursor of the admission service long poll.
CREATE INDEX workspace_build_admissions_updated_at_idx
    ON workspace_build_admissions (updated_at);

COMMENT ON TABLE workspace_build_admissions IS
    'Admission state of START builds on managed templates. A provisioner may acquire a managed START only while its row is granted, unexpired and matches the build.';

COMMENT ON COLUMN workspace_build_admissions.profile IS
    'Resource profile resolved from the template profile parameter when the build was created.';

COMMENT ON COLUMN workspace_build_admissions.reason IS
    'Human readable waiting or decision reason shown to users, for example waiting_for_capacity.';

COMMENT ON COLUMN workspace_build_admissions.queue_position IS
    'Capacity queue position reported by the controller. Unrelated to the provisioner queue position.';

COMMENT ON COLUMN workspace_build_admissions.grant_expires_at IS
    'A granted row stops authorizing acquisition after this time unless the controller refreshes the grant.';

COMMENT ON COLUMN workspace_build_admissions.queued_at IS
    'When the START build entered the capacity queue. The job reaper never measures capacity waits from this time.';

COMMENT ON COLUMN workspace_build_admissions.granted_at IS
    'When the current grant was first recorded. The job reaper measures pending time of granted jobs from here.';

-- Stop builds that the admission controller requested, keyed by the
-- controller operation so retries return the original build.
CREATE TABLE workspace_admission_stop_operations (
    operation_id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    workspace_build_id UUID NOT NULL UNIQUE REFERENCES workspace_builds(id) ON DELETE CASCADE,
    job_id UUID NOT NULL REFERENCES provisioner_jobs(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    canceled_job_id UUID,
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX workspace_admission_stop_operations_workspace_id_idx
    ON workspace_admission_stop_operations (workspace_id);

COMMENT ON TABLE workspace_admission_stop_operations IS
    'STOP builds created through the workspace admission service, keyed by the controller operation ID for idempotent retries.';

COMMENT ON COLUMN workspace_admission_stop_operations.canceled_job_id IS
    'Pending START job that was canceled before it was acquired so the STOP build could be created.';

-- Add workspace_admission scopes for RBAC.
ALTER TYPE api_key_scope ADD VALUE IF NOT EXISTS 'workspace_admission:*';
ALTER TYPE api_key_scope ADD VALUE IF NOT EXISTS 'workspace_admission:create';
ALTER TYPE api_key_scope ADD VALUE IF NOT EXISTS 'workspace_admission:delete';
ALTER TYPE api_key_scope ADD VALUE IF NOT EXISTS 'workspace_admission:read';
ALTER TYPE api_key_scope ADD VALUE IF NOT EXISTS 'workspace_admission:update';
