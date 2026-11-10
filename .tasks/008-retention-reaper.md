---
id: task-008
title: "[Slice] Running jobs are never reaped and MCP retention is 24h"
status: pending
depends_on: ["task-007"]
target_files:
  - "server.go"
  - "main.go"
  - "server_test.go"
---

## Objective

**Objective:** The job reaper never deletes running jobs, expires completed MCP jobs 24 hours after completion, preserves the existing 30-minute UI retention, and evicts the oldest finished MCP jobs when the retained-job cap is exceeded.

## Layer-by-Layer Changes

### Configuration

- Add retention configuration to the MCP/job config: MCP TTL (default 24h), UI TTL (default 30m), reaper interval (default 5m), and maximum retained MCP jobs (default 100).
- Make the clock and ticker injectable for tests; do not use `time.Now`/`time.Tick` directly inside the reaper loop.

### Job Lifecycle

- Fix the current reaper bug:
  - Never delete a job whose status is `queued` or `running`, regardless of age.
  - For MCP jobs, expire only after `finishedAt + MCP TTL`.
  - For UI jobs, preserve the current behavior of 30 minutes after completion; use `finishedAt` when set, otherwise `createdAt`.
  - If `finishedAt` is missing for a terminal job, fall back to `createdAt` and log a warning without panicking.
- Enforce the retained MCP job cap:
  - When the number of finished MCP jobs exceeds the cap, delete the oldest finished jobs first.
  - Never evict a running job to satisfy the cap.
  - Use a deterministic tie-breaker (job ID) when timestamps are equal.
- On deletion, purge the idempotency mapping and any job-owned artifacts/directories (task 009 wires the store cleanup).

### Observability

- Emit a structured retention decision with job ID, owner, status, age, and reason (`ttl`, `cap`, `running_skipped`).
- Do not log page bodies, tokens, or filesystem contents.

### API / Server

- The web UI's visible job lifecycle is unchanged; the only difference is that running UI jobs are no longer deleted mid-crawl.
- `get_job` and `list_jobs` naturally stop returning expired jobs.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `server_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestJobReaperRetention' .
  ```
- **Expected result:** The test MUST fail because the reaper still deletes by `createdAt` and has no owner/TTL/cap rules. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `server.go` — injectable clock/ticker, owner-aware reaper, retained-job cap, idempotency purge.
  - `main.go` — retention config defaults and wiring.
- **Test re-run command:**
  ```bash
  go test -run 'TestJobReaperRetention' .
  ```
- **Expected result:** Tests MUST pass. A running MCP job older than 24h survives; a finished MCP job is removed after 24h; a finished UI job is removed after 30m; a running UI job survives; the cap evicts only the oldest finished MCP jobs; equal timestamps are deterministic.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract a pure `shouldReap(job, now)` decision function and table-test it.
  - Avoid holding the manager lock while deleting files in later slices; collect IDs first, then delete.
  - Confirm no test depends on wall-clock sleeps.
- **Refactor validation command:**
  ```bash
  go test -run 'TestJobReaperRetention' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Running/queued jobs are never reaped
- [ ] MCP TTL is based on `finishedAt`, not `createdAt`
- [ ] UI retention remains 30 minutes and UI job behavior is otherwise unchanged
- [ ] Retained-job cap evicts only finished MCP jobs, oldest first
- [ ] Reaper tests use an injectable clock and do not sleep
