# Cooplay modifications

This repository is a modified version of [Coder](https://github.com/coder/coder),
licensed under the GNU Affero General Public License v3 (see [LICENSE](LICENSE)).
This file is the prominent notice of modification required by section 5(a) of
the license.

- Base: Coder tag `v2.37.6`, commit `c57613f858ba063507e4106ec3a7ce683df4d1d2`.
- Modified by: Cooplay.
- Dates of modification: 2026-10-25, 2026-10-26, 2026-10-29 and 2026-10-30.

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
- RBAC (`coderd/rbac`, `coderd/database/dbauthz`): the `workspace_admission`
  resource, its API key scopes and dedicated `dbauthz` subjects, including a
  read-only subject for the admission of builds a requester may already read,
  and read access for the provisioner daemon.
- Restricted admission API (`coderd/workspaceadmission`, `cli/server.go`,
  `cli/server_admission.go`, `codersdk/deployment.go`): a separate optional
  listener configured with the hidden server options
  `--workspace-admission-listen-address` and
  `--workspace-admission-token-hash-file`, authenticated with a single
  service bearer token.
- Workspace build API (`codersdk/workspacebuilds.go`, `coderd`): workspace
  builds carry an optional `admission` object with `status`, `reason`,
  `queue_position`, `queued_at` and `granted_at`, filled with one batched
  query for single builds, build lists, workspaces, workspace lists and the
  responses of creating a workspace or build. Users have no permission on
  admission records; the rows are read only for builds the requester was
  already authorized to read.
- CLI (`cli/cliui/provisionerjob.go`): commands that wait on a workspace
  build print the capacity wait with its reason and capacity queue
  position, report when capacity is granted, and show the rejection reason.
- Dashboard (`site/src/pages/WorkspacePage`): the workspace page shows
  capacity waits, granted builds waiting for a provisioner and rejections,
  separately from the provisioner queue. The admission service publishes a
  workspace change after grants, requeues and display updates so the page
  stays current.
- Provisioner job metadata (`coderd/provisionerdserver`): builds of managed
  templates never mint or receive the workspace owner's session token and
  never receive the owner's OIDC access token.
- Test infrastructure (`coderd/database/dbtestutil`):
  `CODER_TEST_POSTGRES_PORT` selects the port of the shared test Postgres
  server so tests never use an unrelated server on port 5432.
- Generated API types and documentation that follow from the changes above.

## Agent sleep and wake

Agent workspaces run in a microVM that the platform controller suspends
(container freeze, VM snapshot) and restores while the Coder workspace build
stays running. The agent gets two hooks, driven by the in-VM guest daemon over
the local agent socket, so that a sleeping agent is reported as lost instead
of disconnected and reconnects immediately after restore.

Modified areas:

- Agent reconnect backoff (`agent/agent.go`, `agent/sleepwake.go`): the
  `runLoop` retrier (100 ms to 10 s) is reset to its initial delay after a
  coderd connection stayed up for at least 10 s
  (`DefaultReconnectStableThreshold`; `Options.ReconnectStableThreshold` for
  tests), so one long-lived connection that drops does not start at the
  ceiling.
- PrepareSleep hook (`agent/sleepwake.go`, `agent/agent.go`): marks the
  current and any later coderd connection so that, when it is torn down
  without the agent shutting down, the coordinate stream is closed without
  the graceful `Disconnect` message. The coordinator then reports the agent
  as LOST rather than DISCONNECTED and clients keep the peer. PrepareSleep
  does not tear anything down by itself, and a gracefully shutting down agent
  still sends `Disconnect`. Wake clears it.
- Wake hook (`agent/sleepwake.go`, `tailnet/conn.go`): marks the current
  connection to skip `Disconnect`, closes the coderd RPC connection (each
  `run` attempt dials under its own context, so Wake also aborts a dial
  still in progress and a connection dialed before the wake is discarded),
  and calls the new `tailnet.Conn.Wake`, which closes and redials DERP
  (magicsock `SetNetworkUp(false)` then `true`), rebinds the UDP sockets,
  re-runs STUN and sends a TSMP ping to every known peer. `runLoop` then
  redials immediately with a reset backoff.
- Known limitation: the TSMP ping does not force a new WireGuard handshake.
  wireguard-go only rehandshakes on send when the keypair is missing or
  older than `RejectAfterTime` by the monotonic clock, which does not
  advance while the VM is paused, so a keypair that was fresh at suspend is
  reused until wireguard-go's retry timer (about 5 to 15 seconds) or the
  peer renews it. Forcing it needs a keypair-expiry API in the tailscale
  fork.
- Agent socket (`agent/agentsocket`): `Wake` and `PrepareSleep` RPCs in
  `proto/agentsocket.proto` (regenerated code), served through the
  `SleepWakeHooks` interface (`WithSleepWakeHooks`) and exposed on the
  socket client. The agent API version constant is unchanged.
- CLI (`cli/exp_agent_sleepwake.go`, `cli/root.go`): hidden experimental
  commands `coder exp agent prepare-sleep` and `coder exp agent wake`, with
  `--socket-path` / `CODER_AGENT_SOCKET_PATH` like `coder exp sync`.
- Tailnet lost peer timeout (`tailnet/configmaps.go`, `tailnet/conn.go`):
  the time a LOST peer is kept after its last handshake is configurable with
  `tailnet.Options.LostTimeout` (zero means the default), and the default
  (`tailnet.DefaultLostTimeout`) is raised from 15 minutes to 24 hours. All
  `tailnet.Conn` users, including coderd's server tailnet and CLI clients,
  use the new default. No server flag or deployment option was added.

## Power management of managed workspaces

The platform controller suspends the microVMs of managed workspaces
(paused, hibernated, cold) while their Coder build stays running. Coder
records what clients ask for, so the controller can wake a workspace or
keep it awake, and serves the agent connection state the controller needs.
The controller can also create and delete managed workspaces on behalf of
their owners, and managed workspaces are never stopped on a schedule.

Modified areas:

- Database schema and queries (`coderd/database`): migration `000587`
  adds the `workspace_admission_wake_intents`,
  `workspace_admission_keepalive_intents`,
  `workspace_admission_create_operations`,
  `workspace_admission_delete_operations`,
  `workspace_admission_workspace_states` and `workspace_admission_grants`
  tables with a migration fixture, the intent and agent queries in
  `queries/workspaceadmissionintents.sql` and
  `queries/workspaceadmissions.sql`, the create, delete and grant mirror
  queries in `queries/workspaceadmissionoperations.sql`, the schedule
  queries of managed templates in `queries/workspaceadmissions.sql`, and
  the generated code, mocks, metrics wrappers and `dbauthz` methods. The
  admission change cursor covers admission records, intents and reported
  power states.
- RBAC (`coderd/rbac`, `coderd/database/dbauthz`): a `workspace_wake_recorder`
  subject that can only read managed templates, workspaces and their
  agents and create intent records. It cannot acknowledge intents.
- Wake intents (`coderd/workspaceadmission/wakeintent`,
  `coderd/workspaceadmission_wake.go`, `coderd/workspaceagents.go`,
  `coderd/workspaceapps`, `coderd/coderd.go`): a recorder with a
  non-blocking queue and one worker. Coordinated tunnels on
  `/api/v2/tailnet`, the single-agent coordinate route, web terminals and
  workspace app requests enqueue a request after the requester was
  authorized. The worker skips unmanaged templates, stopped workspaces and
  workspaces that run with a connected agent, writes the first request for
  an agent at once and coalesces repeats within one second. Requests are
  counted in the `coderd_workspace_wake_intents_total` metric by client
  path and result.
- Coder Connect exclusion (`tailnet/service.go`, `tailnet/tunnel.go`): a
  tunnel authorizer can observe that its connection subscribed to
  workspace updates (`tailnet.WorkspaceUpdatesObserver`). Connections that
  do, or that send the Coder Desktop telemetry header, never record wake
  intents.
- Keepalive intents (`coderd/workspaceadmission_keepalive.go`,
  `codersdk/workspacecooplay.go`): `POST
  /api/v2/workspaces/{workspace}/cooplay/keepalive` records a request to
  keep a managed workspace awake for users who may update the workspace.
- Explicit wake (`coderd/workspaceadmission_wake.go`, `coderd/coderd.go`,
  `codersdk/workspacecooplay.go`, `cli/cliui/agent.go`, `cli/ssh.go`,
  `cli/vscodessh.go`, `cli/portforward.go`, `cli/open.go`,
  `cli/speedtest.go`): `POST /api/v2/workspaceagents/{workspaceagent}/wake`
  writes a wake intent at once for users who may connect to the agent and
  answers whether a wake was requested. CLI commands that wait for a
  disconnected or timed out agent call it once before they wait, and ignore
  servers without the route.
- Create and delete on behalf of the owner
  (`coderd/workspaceadmission_create.go`, `coderd/workspaces.go`,
  `coderd/workspaceadmission/create.go`, `cli/server_admission.go`): the
  admission service creates a workspace with the owner's own permissions
  through the regular workspace create path, which gains a hook that runs
  after the build and its admission were inserted in the same transaction.
  Owners that are not active users are refused.
- Grant scope mirror (`coderd/workspaceadmission/admissionapi`,
  `queries/workspaceadmissionoperations.sql`): creates and deletes under a
  platform capability grant check a mirrored copy of its scope (owner,
  expiry, revocation, templates, name prefix, workspace count, delete
  permission) with the grant row locked. Grants carry IDs and limits only,
  never secrets.
- Template schedule (`coderd/schedule/template.go`): managed templates
  report user autostop as disabled and no default TTL, so their builds get
  no deadline and the lifecycle executor never stops them on a schedule.
  Marking a template as managed also turns off its user autostop and
  default TTL and clears the TTLs and build deadlines of its workspaces.
- Restricted admission API (`coderd/workspaceadmission/admissionapi`,
  `cli/server_admission.go`): `GET /v1/wake-intents`,
  `POST /v1/wake-intents/{workspace_id}/ack`, `GET /v1/keepalive-intents`
  and `POST /v1/keepalive-intents/{workspace_id}/ack`, where an
  acknowledgement only applies to the request the controller saw; intent
  events end `GET /v1/watch` long polls; workspaces carry the connection
  state of their agents as recorded in the database. Agents are never
  dialed. `POST /v1/workspaces` creates a workspace on behalf of its
  owner and `POST /v1/workspaces/{workspace_id}/delete` deletes one, both
  idempotent per operation; `PUT /v1/grants/{grant_id}` and
  `POST /v1/grants/{grant_id}/revoke` maintain the grant scope mirror;
  `PUT /v1/templates/{template_id}` also turns off the schedule stops of
  the template.
- Generated API types and documentation that follow from the changes above.
