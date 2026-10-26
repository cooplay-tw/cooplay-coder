# Cooplay modifications

This repository is a modified version of [Coder](https://github.com/coder/coder),
licensed under the GNU Affero General Public License v3 (see [LICENSE](LICENSE)).
This file is the prominent notice of modification required by section 5(a) of
the license.

- Base: Coder tag `v2.37.6`, commit `c57613f858ba063507e4106ec3a7ce683df4d1d2`.
- Modified by: Cooplay.
- Date of modification: 2026-10-25.

The modifications only use and change AGPL-licensed code. No code under
`enterprise/` is used, enabled or modified, and no entitlement checks were
changed. `LICENSE`, `LICENSE.enterprise` and all existing notices are unchanged.

## Workspace admission

Templates can be marked as managed so that an external capacity controller
decides when their workspace START builds may run. Coder keeps its native
build transaction: the workspace, the pending build and a `waiting`
admission record commit together. Only a granted admission lets a provisioner
acquire the job. Capacity waits never occupy provisioner workers and are never
reported as HTTP 403.

Modified areas:

- Database schema and queries (`coderd/database`): migration `000586`
  adds the `workspace_admission_status` enum and the
  `workspace_admission_templates`, `workspace_admission_entitlements`,
  `workspace_build_admissions` and `workspace_admission_stop_operations`
  tables, a migration fixture, the admission queries in
  `queries/workspaceadmissions.sql`, and the generated code, mocks, metrics
  wrappers and `dbauthz` methods.
- Provisioner job acquisition (`coderd/database/queries/provisionerjobs.sql`):
  `AcquireProvisionerJob` only acquires a managed START that has a matching,
  granted and unexpired admission, marks it acquired in the same statement,
  and acquires STOP and DELETE builds before other jobs. Provisioner queue
  positions exclude managed STARTs that wait for capacity.
- Job reaper (`coderd/jobreaper`, `GetProvisionerJobsToBeReaped`): capacity
  waits are never reaped, granted jobs are measured from the grant, and a
  reaped granted job has its admission revoked.
- Build creation (`coderd/wsbuilder`): START builds of managed templates
  resolve the profile parameter, check the owner's entitlement and record a
  waiting admission in the build transaction. Prebuilds are rejected for
  managed templates.
- Build cancellation (`coderd/workspacebuilds.go`): canceling a build that no
  provisioner acquired ends its admission in the same transaction.
- RBAC (`coderd/rbac`): the `workspace_admission` resource, its API key scopes
  and dedicated `dbauthz` subjects.
- Restricted admission API (`coderd/workspaceadmission`, `cli/server.go`,
  `cli/server_admission.go`, `codersdk/deployment.go`): a separate optional
  listener configured with the hidden server options
  `--workspace-admission-listen-address` and
  `--workspace-admission-token-hash-file`, authenticated with a single
  service bearer token.
- Test infrastructure (`coderd/database/dbtestutil`):
  `CODER_TEST_POSTGRES_PORT` selects the port of the shared test Postgres
  server so tests never use an unrelated server on port 5432.
- Generated API types and documentation that follow from the changes above.
