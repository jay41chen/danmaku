# 0016 — Minimal CI that can't pass by testing nothing, and a runtime image with only the binary

**Status**: Accepted (2026-11-16)

## Context

Server tests ran only in a local pre-push hook. That hook can be skipped (`--no-verify`), is off in a
fresh clone until `core.hooksPath` is set, reuses Go's test cache, and runs against a local test database
that earlier runs have already prepared. The integration tests also `t.Skip` when `TEST_DATABASE_URL` or
`TEST_REDIS_URL` is missing, so a CI job with a wrong variable name would be green while testing nothing
that touches Postgres or Redis.

The server image was a single stage on `golang:1.27.1`: 1.87 GB with the toolchain and the source, running
as root. There was no `.dockerignore`, so the build context (23.85 MB) included a stale 22 MB binary that
`.gitignore` hides from git but not from Docker; rebuilding it locally invalidated the `COPY . .` layer,
and a future `.env` in `server/` would have been copied into the image.

There is no deployment at the moment (the Railway trial has expired), so CI is about checking, not gating.

## Decision

**1. One server-only workflow, `server-ci.yml`.** It runs on push to `dev` when `server/**` or the
workflow itself changes, and on manual dispatch. No pull requests (solo development) and nothing waits on
it. The client stays out: `deploy-frontend.yml` already builds it on every client change and doesn't
deploy a failed build.

**2. Real Postgres and Redis as service containers**, and the same command as the hook:
`go test -race -p 1 ./...` (`-p 1` because the store tests share one database).

**3. Missing test variables fail in CI and skip locally.** `testenv.Require(t, name)` returns the value;
if it is missing it calls `t.Fatalf` when `CI` is set (GitHub Actions sets it) and `t.Skipf` otherwise.
It replaced all six "read the variable, skip if empty" blocks. The failure names the missing variable,
and tests that are meant to skip in CI can still call `t.Skip`.

**4. Every test package that uses the database migrates it itself.** The first CI run failed:
`cmd/loadtest` runs before `internal/pgstore`, and on a fresh database its tests hit
`relation "users" does not exist`. Locally they had only ever passed because `pgstore`'s tests had
migrated `danmaku_test` earlier. It was pushed unfixed on purpose to see CI catch it; the fix calls
`pgstore.RunMigration` in the loadtest test helper, after its "only `danmaku_test`" safety check.
Migrations are idempotent and take an advisory lock.

**5. A second job only builds the image**, in parallel, so a broken Dockerfile shows up on push. It also
builds for amd64; the local checks ran on arm64.

**6. A two-stage image.** The build stage compiles with `CGO_ENABLED=0 go build -trimpath`; the runtime
stage is `gcr.io/distroless/static-debian12:nonroot` with only the binary (migrations are embedded with
`go:embed`). Without `CGO_ENABLED=0` the binary links libc dynamically, and on an image without libc it
fails at start with the misleading `exec /server: no such file or directory`. `.dockerignore` is an
allowlist: everything is excluded, then `go.mod`, `go.sum`, `cmd`, `internal` and `migrations` are added
back, so new files never enter the context by default.

## Alternatives considered

- **Fail the CI step if the test output contains `--- SKIP`.** No Go changes and it catches any future
  skip, including a new test that calls `t.Skip` directly. Rejected for a less clear failure (the log
  shows a skip, not which variable is missing) and because every intentional skip would need an
  exception.
- **A migration step in the workflow.** Fixes CI only; a fresh local database would still break.
- **Run the client build in CI too.** Duplicates the deploy workflow's build.
- **No docker job.** The local compose build would catch a broken Dockerfile, but only when someone
  runs it.
- **`alpine` as the runtime image.** Has a shell for debugging, but also a package manager and root by
  default. **`scratch`**: CA certificates and a non-root user would have to be added by hand.
- **A denylist `.dockerignore`.** Easier to read, but anything not listed leaks in.

## Verification

- Locally, `go test -race -p 1 -count=1 ./...` against `danmaku_test` and Redis db 15: 1,014 passed,
  0 skipped.
- Mutation: with `CI=true` and `TEST_REDIS_URL` unset, the Redis tests fail with
  "TEST_REDIS_URL not set in CI"; without `CI` they skip and the package passes.
- On a fresh Postgres container, the loadtest tests failed before the fix with the same error as CI and
  passed after it.
- CI: the first run failed only in `cmd/loadtest`; the second passed (test job 3 min 12 s, docker job
  54 s).
- Image: 1.87 GB → 39.8 MB, build context 23.85 MB → about 521 kB, user 65532, binary statically linked,
  `/healthcheck` returned 200 against the test database.
- Each change was reviewed by a separate agent: the first review found the missing migration; the
  second passed the fix. `actionlint` was not run.

## Consequences

- The image has no shell. Debugging goes through logs and metrics, or a separate debug container.
- The entrypoint moved from `./main` to `/server`. If Railway is re-enabled with a custom start command,
  that command has to change.
- About 3 minutes of CI per server push. Integration tests share one database and run serially; running
  CI jobs in parallel would need a database and Redis db per job.
- A new test that calls `t.Skip` on a missing variable instead of `testenv.Require` is not caught.
- Nothing is gated: a red run is a signal, not a block. Gating deploys is a later change.
