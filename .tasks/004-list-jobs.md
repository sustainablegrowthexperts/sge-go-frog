---
id: task-004
title: "[Slice] Agent lists and filters MCP-created jobs"
status: pending
depends_on: ["task-003"]
target_files:
  - "mcp_tools.go"
  - "mcp_types.go"
  - "server.go"
  - "mcp_test.go"
---

## Objective

**Objective:** An MCP client can list, filter by status and mode, and paginate only MCP-created jobs in newest-first order.

## Layer-by-Layer Changes

### Data / Contracts

- Add `list_jobs` input/output DTOs to `mcp_types.go`:
  - Input: optional `status`, optional `mode`, `limit` (default 20, max 100), `offset`.
  - Output: compact job summaries (same pollable fields as `get_job` minus long fields), `total`, `has_more`, and `next_offset` when `has_more`.
- Reuse the `get_job` status DTO where practical rather than duplicating field mappings.

### Core Logic / Job Manager

- Implement manager `list(filter)` with:
  - Owner filter: only `owner="mcp"` jobs.
  - Optional status filter (`queued`, `running`, `done`, `error`, `cancelled`).
  - Optional mode filter (`spider`, `list`).
  - Newest-first sort by `created_at`; use job ID as a deterministic tie-breaker.
  - Total computed after filtering, before pagination.
  - Stable pagination with `offset` and `limit`.
- Do not include UI jobs even when the caller asks for all statuses.

### MCP / Tool Layer

- Register `list_jobs` in `mcp_tools.go`.
- Validate `status` and `mode` enums; invalid values return `invalid_argument`.
- Clamp or reject `limit > 100`; offset must be non-negative.
- Return structured content and a short text summary with the count and whether more pages exist.

### API / Server

- Keep the web UI without a job list. This tool is MCP-only and must not change any HTML route.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestListJobsMCPOnly' .
  ```
- **Expected result:** The test MUST fail because `list_jobs` and the manager list/filter operation do not exist. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `server.go` — manager `list` filtering, sorting, and pagination.
  - `mcp_tools.go` — register and implement `list_jobs`.
  - `mcp_types.go` — list input/output and compact summary DTOs.
- **Test re-run command:**
  ```bash
  go test -run 'TestListJobsMCPOnly' .
  ```
- **Expected result:** Tests MUST pass. With three MCP jobs and one UI job, the tool returns only the three MCP jobs, newest first; status/mode filters and offset/limit work; `total`, `has_more`, and `next_offset` are correct.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Share the compact-summary mapper with `get_job` where possible.
  - Use one enum-validation helper for status and mode.
  - Remove any direct map iteration that could produce nondeterministic ordering.
- **Refactor validation command:**
  ```bash
  go test -run 'TestListJobsMCPOnly' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] UI jobs are never returned by `list_jobs`
- [ ] Filtering, sorting, `total`, `has_more`, and `next_offset` are covered by tests
- [ ] Invalid status/mode and negative offsets return `invalid_argument`
