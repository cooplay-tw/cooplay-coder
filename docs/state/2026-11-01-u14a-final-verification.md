# U14a final verification

## Final verification, 2026-11-01T02:00:24.682728+08:00

Status: done. All scoped Cooplay verification gates passed. The slim artifact is CLI-only; starting its own server is not applicable. Its expected server rejection and an authenticated client smoke against a server with a fresh database are recorded below. No push, tag or merge.

Test-fix and binary source commit: `363e07ef744b8319af1178c1cdd53bbadf4c8aca` (`test: start Cooplay request contexts after setup`). The subsequent evidence commit changes documentation only. These are untagged verification artifacts, not release artifacts.

Evidence: `/private/tmp/claude-501/-Users-wilson-dev-cooplay-cooplay-internal-platform--claude-worktrees-adoring-nobel-09298b/30c82dc3-2355-490c-bef0-a21f16f4fb3f/scratchpad/wt/coder/_gen/u14a-final3-20261101`. Prior-run transcript: `/private/tmp/claude-501/-Users-wilson-dev-cooplay-cooplay-internal-platform--claude-worktrees-elegant-wiles-13aa39/5222f734-26f7-4344-a440-00c00acbfdbd/scratchpad/codex/runs/CODER-final2.log`. Prior machine-readable test events and results: `/private/tmp/claude-501/-Users-wilson-dev-cooplay-cooplay-internal-platform--claude-worktrees-adoring-nobel-09298b/30c82dc3-2355-490c-bef0-a21f16f4fb3f/scratchpad/wt/coder/_gen/u14a-completion-20261101`.

### Manual checks and commit

PASS: `gofmt -l`, `goimports -l`, repository `scripts/format_go_file.sh` (gofumpt/gci), `golangci-lint run --concurrency=2` (0 issues), `paralleltestctx`, `go run ./scripts/intxcheck`, `go vet -tags=testsmallbatch`, and `git diff --check`. Checks cover the eight changed Go files and packages `./agent`, `./coderd`, `./coderd/workspaceadmission/admissionapi`. Exact arguments and output are in the evidence `.command` and `.log` files. No production code, module file or enterprise code changed. Test assertions and deadline durations are unchanged. The ninth file updates the AGPL modification-date notice.

The installed hook remains `scripts/githooks`. The owner explicitly authorized `git commit --no-verify`; the manual AGPL-safe checks above passed before commit. The full hook was not run and is not represented as passing. Generation and unrelated frontend checks were not repeated for test-only changes.

### Passed tests retained from CODER-final2.log

These completed tests were not rerun. The admission package later hit its 25-minute cap, with 23 passing test events and no failed test events; this is retained as partial coverage, not a successful full-package command.

```text
TestAdmissionChangesNotifyWorkspaceWatchers
TestCancelJob
TestDisplay
TestGrantCancelAcquireRace
TestKeepaliveIntents
TestListPagination
TestNotOnMainListener
TestPutTemplateDisablesAutostop
TestReject
TestStopWorkspace
TestTemplatesAndEntitlements
TestUsers
TestWakeIntents
TestWatch
TestWatchDelayedWakeWithoutNotification
TestWatchWakesOnKeepaliveIntents
TestWatchWakesOnWakeIntents
TestWatchWakesOnWorkspaceState
TestWorkspaceAgents
TestWorkspaceCooplaySource
TestWorkspaceState
TestWorkspaceStateHealth
TestWorkspaceStateInvalid
```

| Prior completed step                       | Result                               | Passing test events    |
|--------------------------------------------|--------------------------------------|------------------------|
| `agent`                                    | PASS                                 | 1                      |
| `agent-agentsocket`                        | PASS                                 | 35                     |
| `agent-agentsocket-proto`                  | PASS                                 | 0                      |
| `agent-agentssh`                           | PASS                                 | 33                     |
| `cli`                                      | PASS                                 | 10                     |
| `cli-cliui`                                | PASS                                 | 127                    |
| coderd proxy/wake/keepalive/data tests     | PASS, schedule alone was outstanding | 33                     |
| Affected site units                        | PASS, 16 files                       | 181                    |
| Affected Storybook interactions            | PASS, 8 files                        | 130                    |
| Site Biome, lint, format, production build | PASS                                 | See prior result files |

### Outstanding tests completed in this pass

The initial remaining-only run used the requested `-race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m` plus `GOMAXPROCS=4`. It produced deadline failures in setup and concurrent requests. A one-CPU isolated control also failed during template import, before route assertions. Both failures are retained in `admission-remaining.jsonl`, `retry-grant-quota.jsonl` and the sample. No deadline extension, assertion removal or production fix was made. The incomplete four-CPU run was stopped after repeated failures.

Passing events from that partial run were retained rather than repeated:

```text
TestAuthentication
TestNewRequiresTokenHash
TestCreateWorkspaceUnderGrant/Mirror
TestCreateWorkspaceUnderGrant/Revocation
TestDeleteWorkspace/Invalid
```

The remaining admission parents were then run individually with the default CPU allowance (`unset GOMAXPROCS`), keeping every requested test flag. Filtered grant and delete runs exclude their already-passed siblings. All successful commands below had zero race warnings; the migration resource failure and its targeted resolution are listed explicitly. This resolves the scoped timing failures through isolated execution; it does not claim a new full-package admission race command passed. The union covers all 31 admission top-level tests and every subtest of the previously incomplete parents.

| Step                                   | Result                                      | Seconds | Passing events |
|----------------------------------------|---------------------------------------------|---------|----------------|
| `admission-lifecycle-isolated`         | PASS                                        | 29.636  | 1              |
| `admission-concurrent-isolated`        | PASS                                        | 21.925  | 4              |
| `admission-create-isolated`            | PASS                                        | 23.72   | 7              |
| `admission-grant-isolated`             | PASS                                        | 26.277  | 4              |
| `admission-delete-isolated`            | PASS                                        | 26.703  | 3              |
| `admission-delete-retry-isolated`      | PASS                                        | 52.9    | 3              |
| `schedule`                             | PASS                                        | 38.677  | 1              |
| `coderd-database`                      | PASS                                        | 10.826  | 15             |
| `coderd-database-dbauthz`              | PASS                                        | 9.422   | 2              |
| `coderd-schedule`                      | PASS                                        | 4.734   | 1              |
| `coderd-workspaceadmission-wakeintent` | PASS                                        | 5.777   | 19             |
| `tailnet`                              | PASS                                        | 14.701  | 4              |
| `admission-core`                       | PASS                                        | 0.47    | 0              |
| `migrations`                           | PARTIAL: 7 resource failures resolved below | 30.377  | 54             |
| `dbauthz-method-suite`                 | PASS                                        | 31.625  | 4484           |
| `migrations-retry`                     | PASS                                        | 18.828  | 19             |

The migration run passed 54 test events, including the complete up/down/up and fixture cases, but seven upstream migration tests failed with `pq: out of shared memory` while using four concurrent tests. Only those seven were retried with `-parallel 2`, all passing. The initial failed command is not presented as green; combined coverage is complete. No database configuration or source change was needed. The additional full `TestMethodTestSuite` verifies authorization and method accounting (4,484 passing events).

Exact commands, after sourcing the recorded mise environment and setting `CODER_TEST_POSTGRES_PORT=49470`:

```sh
unset GOMAXPROCS
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/workspaceadmission/admissionapi -run '^(TestRequestLifecycle)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/workspaceadmission/admissionapi -run '^(TestCreateWorkspaceConcurrent)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/workspaceadmission/admissionapi -run '^(TestCreateWorkspace)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/workspaceadmission/admissionapi -run '^TestCreateWorkspaceUnderGrant$/^(Quota|Templates|Denied)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/workspaceadmission/admissionapi -run '^TestDeleteWorkspace$/^(Employee|UnderGrant)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/workspaceadmission/admissionapi -run '^(TestDeleteWorkspaceConcurrentRetry)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd -run '^(TestManagedTemplateSchedule)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/database -run '^(TestWorkspaceAdmissionCooplaySource|TestWorkspaceAdmissionCreateOperations|TestWorkspaceAdmissionDelayedWakeWatermark|TestWorkspaceAdmissionGrants|TestWorkspaceAdmissionKeepaliveIntents|TestWorkspaceAdmissionTemplateSchedule|TestWorkspaceAdmissionWakeIntentAckRace|TestWorkspaceAdmissionWakeIntentConstraints|TestWorkspaceAdmissionWakeIntentListOrderAndLimit|TestWorkspaceAdmissionWakeIntents|TestWorkspaceAdmissionWakeTarget|TestWorkspaceAdmissionWatermarkCommitOrder|TestWorkspaceAdmissionWorkspaceAgents|TestWorkspaceAdmissionWorkspaceStateConstraints|TestWorkspaceAdmissionWorkspaceStates)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/database/dbauthz -run '^(TestAsWorkspaceWakeRecorder|TestDBAuthzRecursive)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/schedule -run '^(TestAGPLTemplateScheduleStoreManagedTemplate)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/workspaceadmission/wakeintent -run '^(TestNilRecorder|TestRecorderDebouncesRepeats|TestRecorderEnqueueNeverBlocks|TestRecorderRecord|TestRecorderSkipConnect|TestRecorderSkips|TestRecorderWritesFirstIntentImmediately)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./tailnet -run '^(TestConn_ForwardedConnCount|TestConn_Wake|TestWorkspaceUpdates|TestWorkspaceUpdatesObserver)$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/workspaceadmission
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 4 -timeout 25m ./coderd/database/migrations
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 2 -timeout 25m ./coderd/database/dbauthz -run '^TestMethodTestSuite$'
go test -json -race -tags=testsmallbatch -count=1 -p 1 -parallel 2 -timeout 25m ./coderd/database/migrations -run '^(TestMigration000457ChatAccessRole|TestMigration000504AIProvidersBackfill|TestMigration000543ChatSearchSchemaBehavior|TestMigration000556UserSecretsEnabled|TestMigration000563TemplateAgentsAllowedBackfill|TestMigration000580ChatModelConfigOrganization|TestMigration000583ChatModelOverrideOrgScope)$'
```

The database checks use the repository helper, isolated databases and the existing owned PG18 container `u14a-final-20261101-pg18` at random loopback port 49470. Image: `postgres@sha256:979bccfde35445db618c65107d27d08c6c21339d8dc3ad4c830c6633947be0e4`; PostgreSQL 18.6. No shared/personal database or host port 5432/5433 was used. Migration/fixture coverage completed under race across the retained passing cases and the seven-test retry against this instance.

### Verification build and provenance

Clean detached worktree: `/private/tmp/claude-501/-Users-wilson-dev-cooplay-cooplay-internal-platform--claude-worktrees-adoring-nobel-09298b/30c82dc3-2355-490c-bef0-a21f16f4fb3f/scratchpad/coder-verify-final3`. Enterprise excluded with sparse checkout. Reused installed Node dependencies are ignored links; the frontend was rebuilt. Go dependency audits for linux/amd64 embed, linux/amd64 slim and darwin/arm64 embed found zero `github.com/coder/coder/v2/enterprise` packages. The unrelated dependency `github.com/googleapis/enterprise-certificate-proxy` is not Coder enterprise code.

The initial Node install prompt stopped the first attempt; the final command treats existing install sentinels as up to date. Go 1.26.9 local source (`cmd/go/internal/vcs/vcs.go`, `vcsGit.RootNames`) recognizes `.git` directories, not the worktree `.git` file. The verification worktree `.git` is therefore a symlink to the same existing worktree metadata directory. Git status remained clean. Initial unstamped binaries were replaced; no unstamped artifact is listed below.

```sh
export CODER_BUILD_AGPL=1
export GOFLAGS='-p=2 -buildvcs=true'
unset CODER_RELEASE
make -j4 -o node_modules/.installed -o site/node_modules/.installed 'OS_ARCHES=linux_amd64 darwin_arm64' build/coder_linux_amd64 build/coder-slim_linux_amd64 build/coder_darwin_arm64
```

Build version: `2.38.2-devel+363e07ef74`. Toolchain: `go version go1.26.9 darwin/arm64`. `go version -m` verified the exact source revision, `vcs.modified=false`, AGPL linker setting, `CGO_ENABLED=0`, expected OS/architecture and embed/slim tags on all three binaries. Full metadata is in `build-metadata.txt`; digests and provenance are in `SHA256SUMS` and `binary-provenance.json`.

| Artifact                 | SHA256                                                             |
|--------------------------|--------------------------------------------------------------------|
| `coder_linux_amd64`      | `c9a25045eab1bd302879002486bdb3d08bb592b45fd1231578513622ce6f11c2` |
| `coder-slim_linux_amd64` | `938e36a10b74fd6444d7631dad64cb6af3bfbc4b3ac5052a5e9a4c3f054a3f7f` |
| `coder_darwin_arm64`     | `2fc80b31b97c8cc1865994d87590528320fc61917530b4085fbba535ec13e82b` |

### Binary smoke results

| Artifact                           | Result                                                                                                                       |
|------------------------------------|------------------------------------------------------------------------------------------------------------------------------|
| darwin/arm64 server                | PASS: version, sessions help, fresh database migrations, `/api/v2/buildinfo`, `/healthz` 200, graceful exit 0                |
| linux/amd64 server                 | PASS: same checks, using userspace QEMU on the arm64 Docker host                                                             |
| linux/amd64 slim                   | PASS: version, sessions help, authenticated `list --output json` returns `[]` against a server backed by fresh PG18 database |
| linux/amd64 slim standalone server | NOT APPLICABLE: upstream slim explicitly excludes `server`; measured rejection retained in `linux-slim-server.log`           |

Linux execution used the existing native arm64 image `devws-review-u08/qemu-helper:local` via `/usr/bin/qemu-x86_64`, pinned by its image ID in `linux-emulation-image.json`. No privileged binfmt installation or host change. Fresh databases: `u14a_final3_vcs_darwin_smoke`, `u14a_final3_vcs_linux_fat_server`, and `u14a_final3_slim_client`. The initial unsupported slim server attempt also created its own unused fresh database. Own smoke servers, networks and the five databases created by smoke attempts were removed; verification binaries and logs are retained. No other containers or worktrees were modified.

Exact smoke commands are in `*-version.command`, `*-sessions-help.command`, `darwin-server.command`, `linux-fat-server-start.command`, and `linux-slim-client.commands.json`; HTTP observations are in `*-buildinfo.json` and `smoke-results.json`. The client harness initially parsed a development-version notice as JSON; the command had already exited 0 and returned `[]`. The parser was corrected without rerunning the successful command.

### Handoff

Verification is complete for this authorized scope. The external `design/handover-U14a.md` receives this same dated section; because that notes directory is not a Git repository, this dated evidence file is the committed copy. The release tag and release rebuild remain separate actions for the decision maker. No push or tag was performed.
