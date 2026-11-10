---
id: task-015
title: "[Slice] Agent reads strict and inclusive audit sections"
status: completed
depends_on: ["task-014"]
target_files:
  - "mcp_audit.go"
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp_test.go"
  - "audit_integration_test.go"
---

## Objective

**Objective:** An MCP client can read strict or inclusive audit sections with deterministic sorting, section-specific filters, pagination, and clear `audit_not_available` errors.

## Layer-by-Layer Changes

### Audit Loader

- Extend `mcp_audit.go` to load `audit.json` from the job-owned strict or inclusive directory.
- Cache decoded results by path + mtime; invalidate when the file changes.
- Guard against path traversal: the handler never accepts a path, only a job ID and profile; the loader builds the path from the job.
- Return `audit_not_available` when:
  - The job did not enable audit.
  - The selected profile is not ready.
  - The inclusive profile failed; include the stored error text without leaking paths.
  - The job is not done.

### Sections

- Support `profile` `strict` (default) and `inclusive`.
- Support `section`:
  - `summary` — `AuditSummary` plus high-level graph counts.
  - `pages` — paginated `Result.Pages` map, sorted deterministically.
  - `links` — content page links from `Result.Links` only.
  - `blocks` — `Result.Blocks`, including excluded decisions.
  - `anchors` — `Result.Anchors` aggregates.
  - `pagerank` — `Result.PageRankRanking` plus `PageRankMeta`.
  - `redirects` — `Result.Redirects`.
  - `failed` — `Result.Failed`.
  - `sitemap` — `Result.Sitemap`.
  - `structure` — `Result.Structure`.
  - `all` — summary and section availability/counts only; never dump every array.
- For `pages`, expose `page_key` and the URL fields `url`, `raw_url`, and `final_url` when present.
- `links` remains content-only. Document the external/redirect/excluded caveat and point callers to the audit ZIP artifact for the full inventory.

### Pagination / Filters

- Default `limit=100`, maximum `500`; `offset` non-negative.
- Sort options are section-specific:
  - `pages`: URL, content inbound, PageRank, orphan status.
  - `links`/`blocks`: source/target or block ID with a deterministic tie-breaker.
  - `pagerank`: rank descending by default.
- Filters:
  - `pages`: `orphan_only`, status, maybe PageRank threshold if defined by the section.
  - `blocks`/`links`: `decision`.
  - Reject unknown filter keys with `invalid_argument`.
- Pagination envelope: `total` after filtering, `has_more`, `next_offset`, `truncated` when a server cap stops output.

### MCP / Tool Layer

- Register `get_audit` in `mcp_tools.go`.
- Validate `job_id`, `profile`, `section`, `sort`, `limit`, and `filter`.
- Return structured content plus a short text summary.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestGetAuditSections' .
  ```
- **Expected result:** The test MUST fail because `get_audit` and the section loader do not exist. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp_audit.go` — profile-aware loading, caching, section extraction, sorting, filters, pagination.
  - `mcp_tools.go` — register and implement `get_audit`.
  - `mcp_types.go` — audit input/output and section envelopes.
- **Test re-run command:**
  ```bash
  go test -run 'TestGetAuditSections' .
  ```
- **Expected result:** Tests MUST pass. With synthetic strict and inclusive `audit.json` fixtures, the tool returns summary, pages, links, blocks, anchors, PageRank, redirects, failed, sitemap, structure, and `all`; pages sort and paginate deterministically; `links` contains content rows only; a disabled/failed profile returns `audit_not_available`; unknown sections/filters return `invalid_argument`.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Use one section-dispatch table instead of a long switch if it improves clarity.
  - Share pagination and deterministic-sort helpers across sections.
  - Verify cache invalidation when `audit.json` mtime changes.
- **Refactor validation command:**
  ```bash
  go test -run 'TestGetAuditSections' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] All listed sections and both profiles are covered
- [ ] `pages` is sorted deterministically before pagination and exposes the URL identity fields
- [ ] `links` returns content links only, with the caveat documented
- [ ] `all` returns counts/availability, not full arrays
- [ ] Disabled, not-ready, and failed profiles return `audit_not_available`
