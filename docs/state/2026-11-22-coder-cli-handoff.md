# CACC-21 local verification, 2026-11-22

The Coder fork worktree started at `4525e79f45` (`v2.37.6-cooplay.1`) on `claude/cacc-21-cli-handoff`.
Verification used Go 1.26.9 on darwin/arm64, an offline module cache, in-process HTTP requests, and `dbmock`.
This record covers local verification and a configuration review.

## Handoff checks

- The capability probe returns version 1 without a database lookup, including when a session cookie is present.
- A valid browser session can issue a code and receives a 303 to the exact loopback callback with `no-store` and `no-referrer`.
  A signed-out browser receives a login redirect with the original path and query.
  The trace for that request omits the handoff values.
- Invalid parameters return a plain-text 400 before session authentication.
  Suspended and system users, and credentials without permission to create an API key, cannot issue a code.
- A successful exchange returns a password-type CLI key with the configured default lifetime and request IP, and records API key creation for the issuing user.
  It returns the key in a 201 JSON body with `no-store`.
- Replay, expiry, mismatched callbacks, incorrect or malformed verifiers, unknown codes, and ineligible users return the same 400 response.
  Concurrent consumption has one winner.
  The store caps live entries at 1,000, prunes expired entries on insert, and expires codes after 60 seconds.
  The token handler rejects oversized bodies and applies a per-IP limit.

## Local commands

- `go test ./coderd/ -run 'Cooplay.*Handoff|CLIHandoff' -count=1` passed.
- `go test -race ./coderd/ -run 'Cooplay.*Handoff|CLIHandoff' -count=1` passed.
- `go vet ./coderd/ ./coderd/apidoc` passed.
- Diff-scoped `golangci-lint` for `./coderd` reported zero issues.
- `go build -o "$TMPDIR/cacc-21-coder" ./cmd/coder` passed for the AGPL entry point.
  Go reported a sandbox-denied stat cache write and exited successfully.
- The offline Swagger generator added three paths and two schemas without changing existing entries.
- `make lint/emdash` and whitespace checks passed.
  The Coder worktree has no `scripts/check-docs.py`.
  Running the platform copy against Coder reported 15 existing relative-link issues elsewhere in the fork.
  A check limited to the changed Markdown files found both relative file links valid.

## Workspace ingress review

Source is the platform repo's local `origin/master` ref at `084c02e9f356`, read using `git show`.
Both templates, `services/agent-workspaces/templates/go-hair-core-linux/main.tf` and `services/agent-workspaces/templates/pikki-dev/main.tf`, set `agent_url` to the machine ingress, `https://coder.devws.access.cooplay.tw` by default.
`services/agent-workspaces/deploy/render.py` points ingress at the loopback Coder origin.
`services/agent-workspaces/internal/ingress/allowlist.go` limits Coder traffic to the pinned agent routes, and `proxy.go` returns 403 for a path outside that allowlist.
The probe, authorization, and exchange paths all fall outside the allowlist.
The employee Teleport app is a separate entry point.
For an exchange that reaches Coder, the code, original callback, and matching verifier are still required.

## Remaining verification

The supervisor owns the release build and deployment.
Live Teleport routing, deployed guest ingress, and the one-tab browser acceptance scenarios still require verification.
The sandbox denied creation of the Git worktree index lock, so the diff remains in the worktree for review and commit.
The endpoint contract and rollback are in [Cooplay changes](../../COOPLAY-CHANGES.md#cooplay-cli-session-handoff).
