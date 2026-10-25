-- Enum additions to api_key_scope are intentionally not reversed
-- because Postgres cannot drop enum values safely.

DROP TABLE IF EXISTS workspace_admission_stop_operations;
DROP TABLE IF EXISTS workspace_build_admissions;
DROP TABLE IF EXISTS workspace_admission_entitlements;
DROP TABLE IF EXISTS workspace_admission_templates;
DROP TYPE IF EXISTS workspace_admission_status;
