---
id: task-006
title: "[Slice] Repeated start_crawl requests are idempotent"
status: completed
depends_on: ["task-005"]
target_files:
  - "server.go"
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp_test.go"
---

## Objective

**Objective:** A repeated `start_crawl` with the same `client_request_id` returns the original job, while the same key with different input returns a stable `conflict` error.

## Layer-by-Layer Changes

### Data / Contracts

- Add optional `client_request_id` to the `start_crawl` input DTO.
- Store the request ID on the job and in an owner-scoped idempotency index.
- Add a stable canonical fingerprint of the effective request: mode, start URL or sorted/deduplicated list URLs, keywords, concurrency, audit, audit filters, and limits.

### Core Logic / Job Manager

- Add an idempotency map to the job manager:
  - Key: `owner + "\x00" + client_request_id`.
  - Value: job ID plus the canonical input fingerprint.
- On `start_crawl` with a request ID:
  - If no mapping exists, create the job and record the mapping.
  - If the mapping exists and the fingerprint matches, return the existing job's start response with the same `job_id`.
  - If the mapping exists and the fingerprint differs, return `conflict` with `retryable=false` and details naming the existing job ID.
- Without a `client_request_id`, always create a new job.
- Purge the mapping when the job is deleted or reaped so the index does not grow forever.

### MCP / Tool Layer

- Validate `client_request_id` as a non-empty string when supplied; enforce a reasonable length cap (for example 128 characters).
- Return the same `job_id`, status, phase, and links on replay.
- Do not expose other callers' request IDs or job data.

### API / Server

- The web UI does not send `client_request_id`; its behavior must not change.
- The idempotency index is in-memory in this slice; task 009 persists job data but does not need to persist the index for v1.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestStartCrawlIdempotency' .
  ```
- **Expected result:** The test MUST fail because `client_request_id` and the idempotency index do not exist. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `server.go` — idempotency map, fingerprint storage, lookup/purge hooks.
  - `mcp_tools.go` — request-ID validation and replay/conflict behavior.
  - `mcp_types.go` — request field and conflict details.
- **Test re-run command:**
  ```bash
  go test -run 'TestStartCrawlIdempotency' .
  ```
- **Expected result:** Tests MUST pass. Two identical requests return one job and the same ID; a different URL with the same key returns `conflict`; requests without a key create two distinct jobs; the map is purged when the job is deleted.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract canonical fingerprint construction into one pure function with a table-driven test.
  - Ensure the map is guarded by the manager mutex and has no race with deletion.
  - Verify replay does not launch a second crawl.
- **Refactor validation command:**
  ```bash
  go test -run 'TestStartCrawlIdempotency' . && go test -race ./...
  ```
- **Expected result:** All previously passing tests still pass under the race detector. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Identical replay returns the existing job and does not launch a duplicate crawl
- [ ] Same key with different input returns `conflict`
- [ ] Missing key always creates a new job
- [ ] Idempotency entries are removed when jobs are deleted
