---
id: task-009
title: "[Slice] Completed MCP jobs survive restarts"
status: pending
depends_on: ["task-008"]
target_files:
  - "job_store.go"
  - "job_store_test.go"
  - "server.go"
  - "main.go"
---

## Objective

**Objective:** Completed MCP jobs, page rows, and artifacts persist under per-job directories and are restored after a restart, while previously running jobs are marked `error` with code `interrupted`.

## Layer-by-Layer Changes

### Data / Store

- Create `job_store.go` with a per-job directory layout under the configured results directory:
  ```text
  results/jobs/<job_id>/
  ├── job.json          # settings, status, phase, timestamps, owner, limits, summaries
  ├── pages.json.gz     # page rows
  ├── report.csv        # generated artifact (later slice)
  └── audit/            # strict/inclusive audit directories (later slice)
  ```
- Use atomic writes: write to a temporary file in the same directory, flush/close, then rename over the target. Never write partial JSON.
- Persist at least:
  - job ID, mode, target, owner
  - status, phase, created/started/finished timestamps
  - progress count, current URL, keywords
  - effective limits, truncated, limit reason
  - archive flag/directory
  - audit enablement, state, summaries, strict/inclusive directories
  - error message/code
  - client request ID and settings needed to rebuild the job
- Add load, delete, cleanup-expired, and load-all operations. Validate that a loaded job ID matches its directory; reject or skip malformed entries without crashing the server.
- Guard against path traversal: job IDs are generated hex strings and must be validated before building paths.

### Job Lifecycle

- Persist a job at creation (status `running`/`queued`) so a crash can be recovered.
- Persist on every terminal transition and on meaningful phase changes; do not write on every progress callback.
- On startup with persistence enabled:
  - Load completed jobs and make them available to `get_job`, `list_jobs`, and later read tools.
  - Mark previously `queued`/`running` jobs as `error` with code `interrupted`, set `finishedAt`, and persist the change.
  - Leave completed jobs intact.
- The reaper uses the store's delete/cleanup operations; never delete a running job.

### Config / Server

- Wire the results directory and persistence toggle through `main.go` and the shared app constructor.
- If the results directory is not writable, fail startup with a clear error instead of silently running without persistence.
- Keep UI jobs process-local unless persistence is explicitly enabled for them; v1 persistence is for MCP jobs.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `job_store_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestJobStore' .
  ```
- **Expected result:** The test MUST fail because `job_store.go` and the persistence hooks do not exist. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `job_store.go` — per-job directories, atomic writes, load/delete/cleanup, startup recovery.
  - `server.go` — persist on create/terminal/phase changes, hydrate on startup, wire reaper cleanup.
  - `main.go` — results directory and persistence configuration.
- **Test re-run command:**
  ```bash
  go test -run 'TestJobStore' .
  ```
- **Expected result:** Tests MUST pass. A saved job and its page rows load into a new manager; `get_job`/`list_jobs` see it; a job saved as `running` loads as `error`/`interrupted`; malformed files are handled; expired completed jobs are cleaned; no `.tmp` files remain after a successful write.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract atomic JSON and gzip helpers.
  - Keep the persisted schema versioned (`version` field) so future changes are explicit.
  - Ensure no filesystem path from the store appears in MCP responses.
- **Refactor validation command:**
  ```bash
  go test -run 'TestJobStore' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] `go test -race ./...` passes for persistence and hydration
- [ ] Completed MCP jobs survive a simulated restart and are readable through the tools
- [ ] Previously running jobs become `error`/`interrupted` on startup
- [ ] Writes are atomic and job IDs are validated before path construction
- [ ] Reaper cleanup deletes job directories only for expired, non-running jobs
