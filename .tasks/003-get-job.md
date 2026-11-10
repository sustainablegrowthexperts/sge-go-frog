---
id: task-003
title: "[Slice] Agent polls job status, phase, and progress"
status: completed
depends_on: ["task-002"]
target_files:
  - "mcp_tools.go"
  - "mcp_types.go"
  - "server.go"
  - "mcp_test.go"
---

## Objective

**Objective:** An MCP client can poll `get_job` and observe status, phase, timestamps, page progress, current URL, keywords, limits, truncation, audit state, artifact readiness, and errors for an MCP-created job.

## Layer-by-Layer Changes

### Data / Contracts

- Add the `get_job` input/output DTOs to `mcp_types.go`:
  - `job_id`
  - `mode`, `target`
  - `status`, `phase`
  - `created_at`, `started_at`, `finished_at` (RFC 3339 UTC; `null` when not applicable)
  - `pages_crawled`, `current_url`
  - `keywords`
  - `truncated`
  - `limits` (`max_pages`, `max_depth`, `max_duration_seconds`)
  - `audit` (`enabled`, `state`, `strict`, `inclusive`)
  - `error`
  - `artifacts` (`csv_ready`, `audit_strict_ready`, `audit_inclusive_ready`)
- Audit state values: `disabled`, `pending`, `running`, `ready`, `error`, `partial`.

### Core Logic / Job Manager

- Add a concurrency-safe job snapshot method that copies all pollable fields under the job mutex. Do not read mutable job fields directly from the handler.
- Map the internal `crawlJob` fields to the DTO:
  - `pages_crawled` is the progress count, not necessarily `len(result)`.
  - `current_url` may be empty before the first response and after completion.
  - `keywords` comes from the parsed keyword list so the agent knows which keys appear in later results.
  - `limits` reflects the effective per-job limits.
  - `truncated` and any limit reason are exposed.
  - Audit failures are reported separately and do not turn the crawl into an `error`.
  - `artifacts.csv_ready` is true when the job is done and pages are available; audit readiness reflects strict/inclusive state.

### MCP / Tool Layer

- Register `get_job` in `mcp_tools.go`.
- Require `job_id`; return `invalid_argument` when missing.
- Look up only MCP-created jobs. A UI job ID or unknown ID returns `job_not_found` (do not disclose whether a UI job exists).
- Return the DTO as structured tool content plus a short text summary suitable for an agent.

### API / Server

- Keep the existing `/api/crawl/{id}` HTML fragment polling untouched. `get_job` is a separate read path.
- Do not add results, audit, search, or artifact payloads to this tool.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestGetJobStatus' .
  ```
- **Expected result:** The test MUST fail because `get_job` is not registered and the job snapshot contract does not exist. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp_tools.go` — register and implement `get_job`, error mapping for missing/UI jobs.
  - `mcp_types.go` — job-status, audit-state, and artifact-readiness DTOs.
  - `server.go` — thread-safe job snapshot and phase/timestamp updates.
- **Test re-run command:**
  ```bash
  go test -run 'TestGetJobStatus' .
  ```
- **Expected result:** Tests MUST pass. A running fake job reports `status=running`, a live phase, progress, current URL, keywords, limits, and readiness flags; after release it reports `done` and a non-null `finished_at`; unknown IDs and UI IDs return `job_not_found`.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract timestamp/limit/audit-state mapping into one helper to keep the handler small.
  - Ensure every read is under the job mutex.
  - Verify no filesystem paths or proxy credentials appear in the response.
- **Refactor validation command:**
  ```bash
  go test -run 'TestGetJobStatus' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Running, done, and error states are all covered by tests
- [ ] Timestamps are RFC 3339 UTC; durations and limits use the spec units
- [ ] UI jobs and unknown job IDs are indistinguishable through `get_job`
