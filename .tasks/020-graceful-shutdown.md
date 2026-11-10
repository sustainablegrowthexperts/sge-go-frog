---
id: task-020
title: "[Slice] SIGINT/SIGTERM shuts down MCP work cleanly"
status: pending
depends_on: ["task-019"]
target_files:
  - "main.go"
  - "server.go"
  - "mcp.go"
  - "spider.go"
  - "list.go"
  - "server_test.go"
  - "mcp_test.go"
---

## Objective

**Objective:** SIGINT/SIGTERM stops new MCP work, marks running jobs as interrupted, closes archive writers without half-written files, and shuts down the HTTP server within a bounded timeout.

## Layer-by-Layer Changes

### Signal Handling

- In `main.go`, use `signal.NotifyContext` for `SIGINT` and `SIGTERM`.
- Run the server in a goroutine or with a blocking `Serve` that can be shut down.
- On signal:
  - Set the app to a shutting-down state so new `start_crawl` calls are rejected.
  - Cancel runner contexts.
  - Mark queued/running jobs as `error` with code `interrupted`.
  - Close archive writers and flush persistence.
  - Call `http.Server.Shutdown` with a configured timeout (for example 30 seconds).
  - Exit non-zero only when shutdown fails.

### App / Job Manager

- Add an `App.Shutdown(ctx)` method that is idempotent.
- New start requests during shutdown return a stable, retryable error (`limit_exceeded` with a shutdown message or `internal_error` with `retryable=true`); document the choice and keep it consistent.
- Set `finishedAt` and persist the interrupted state for every running/queued MCP job.
- Do not reap or delete jobs during shutdown.
- Close the MCP server/session handler after in-flight calls finish or the timeout expires.

### Crawl Pipeline

- Thread the runner context through `runSpiderContext`/`runListContext`.
- Check `ctx.Err()` before enqueueing new URLs and in callbacks. In-flight requests may finish up to the client timeout.
- Ensure `defer archive.Close()` runs on cancellation; do not leave `manifest.json.tmp` or half-written archive files.
- Preserve partial page rows if they are already recorded; mark the job interrupted rather than done.

### Persistence

- Save interrupted jobs before the process exits.
- On the next startup, recovery must not overwrite an already-terminal interrupted state.

### Tests

- Use a blocking fake runner to keep a job running.
- Call `Shutdown` directly (no real OS signal in unit tests) and assert:
  - Running job becomes `error`/`interrupted`.
  - New `start_crawl` is rejected.
  - The HTTP listener/session is closed or no longer accepts.
  - No `.tmp` files remain in the job/archive directory.
- Add a signal-level test only if it can be made deterministic; otherwise test the shutdown path directly.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `server_test.go` and `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestGracefulShutdown' .
  ```
- **Expected result:** The test MUST fail because `Shutdown`, shutdown state, and context cancellation do not exist. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `main.go` — signal context and shutdown orchestration.
  - `server.go` — `App.Shutdown`, shutting-down state, interrupted job persistence, HTTP shutdown.
  - `mcp.go` — close MCP sessions/server.
  - `spider.go`, `list.go` — context checks and clean archive close.
- **Test re-run command:**
  ```bash
  go test -run 'TestGracefulShutdown' .
  ```
- **Expected result:** Tests MUST pass. Shutdown marks the running job interrupted, persists it, rejects new starts, closes the listener, and leaves no temporary manifest/archive files.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Make shutdown idempotent and safe under concurrent calls.
  - Extract a `shutdownTimeout` config value.
  - Verify no goroutine leaks after shutdown using a test that waits for the runner to exit.
- **Refactor validation command:**
  ```bash
  go test -run 'TestGracefulShutdown' . && go test -race ./...
  ```
- **Expected result:** All previously passing tests still pass under the race detector. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] SIGINT and SIGTERM both trigger the shutdown path
- [ ] Running/queued MCP jobs become `error`/`interrupted` and are persisted
- [ ] New starts are rejected during shutdown
- [ ] Archives close cleanly with no `.tmp` or half-written files
- [ ] HTTP shutdown is bounded by a timeout and is idempotent
