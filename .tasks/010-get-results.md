---
id: task-010
title: "[Slice] Agent retrieves filtered and paginated crawl results"
status: pending
depends_on: ["task-009"]
target_files:
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp_audit.go"
  - "mcp_test.go"
---

## Objective

**Objective:** An MCP client can retrieve filtered, sorted, and paginated page rows for a completed job, including keyword counts, summary counts, manifest depth/discovery, and an optional strict-audit join.

## Layer-by-Layer Changes

### Data / Contracts

- Add `get_results` input/output DTOs to `mcp_types.go`:
  - Input: `job_id`, `status_code` (single int or array), `url_contains`, `title_contains`, `keyword`, `min_keyword_hits`, `sort`, `order`, `offset`, `limit`, `include_audit`.
  - Output: `job_id`, `total`, `offset`, `limit`, `has_more`, `next_offset`, `summary`, and `rows`.
  - Row: `url`, `status_code`, `load_time_ms`, `parent_url`, `inlinks_raw`, `title`, `description`, `h1s`, `img_alts_raw`, `robots`, `keywords`, optional `depth`, optional `discovered_via`, optional `audit`.
- Summary: `pages`, `by_status`, `keywords`, `truncated`.

### Core Logic / Results

- Require `status == "done"`; return `job_not_finished` for running/error jobs. Do not return partial rows from a running job.
- Read page rows from the job's persisted result (or in-memory result for UI-independent tests).
- Filters:
  - `status_code` exact match, single or multiple.
  - `url_contains` and `title_contains` case-insensitive substring.
  - `keyword` must be one of the job's parsed keywords; otherwise `invalid_argument`.
  - `min_keyword_hits` applies only with `keyword`.
- Sorting: `url`, `status_code`, `load_time`, `keyword_hits`; default order ascending for URL and descending for numeric fields. Apply a deterministic tie-breaker by URL.
- Pagination: `offset` default 0; `limit` default 100 with a maximum of 500; compute `total` after filtering; `has_more` and `next_offset` from the filtered set.
- Keyword map: map each parsed keyword to the row's `KeywordHits` value in keyword order; missing counts are 0.
- `load_time_ms` converts `Page.LoadTime` to integer milliseconds.
- `inlinks_raw` and `img_alts_raw` preserve the current CSV serialization.
- Depth/discovery: when the job has an archive directory with `manifest.json`, look up each row by URL and expose `depth` and `discovered_via`; otherwise omit both fields.
- `include_audit=true`:
  - Requires a ready strict audit; otherwise return `audit_not_available`.
  - Load strict `audit.json` through `mcp_audit.go` and join by normalized URL (`audit.CanonicalURL`), never by raw string.
  - Add `page_key`, content inbound/outbound, PageRank, orphan flags, reachability, and sitemap flags per row.
- Response cap: stop adding rows when the serialized response would exceed the configured cap (default 5 MB), set `truncated=true`, and keep `has_more` accurate for the underlying filtered set.

### MCP / Tool Layer

- Register `get_results` in `mcp_tools.go`.
- Validate enums and numeric bounds; return `invalid_argument` naming the field.
- Return structured content plus a short text summary with total rows and page window.

### Audit Loader (initial)

- Create `mcp_audit.go` with a small, path-safe strict-audit loader: read `audit.json` from the job-owned directory, cache by path + mtime, and expose the decoded `auditpkg.Result`. Task 015 extends this into sectioned reads.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestGetResults' .
  ```
- **Expected result:** The test MUST fail because `get_results`, filtering/sorting/pagination, and the audit join do not exist. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp_tools.go` — register and implement `get_results`.
  - `mcp_types.go` — results DTOs, filters, row, and summary.
  - `mcp_audit.go` — strict audit loader and normalized-URL join helper.
- **Test re-run command:**
  ```bash
  go test -run 'TestGetResults' .
  ```
- **Expected result:** Tests MUST pass. A completed job returns filtered rows with correct totals, keyword maps, sorting, pagination, and summary; a running job returns `job_not_finished`; manifest depth/discovery appears when an archive exists; `include_audit` joins by canonical URL; oversized output sets `truncated`.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract filtering, sorting, and pagination into pure helpers with table-driven tests.
  - Keep the response-size accounting in one place.
  - Ensure no raw filesystem path or audit directory appears in the response.
- **Refactor validation command:**
  ```bash
  go test -run 'TestGetResults' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Filters, sorting, pagination, keyword map, and summary counts are covered
- [ ] Running/error jobs return `job_not_finished`
- [ ] Depth/discovery come from the manifest and are omitted when no archive exists
- [ ] Audit join uses normalized URLs and returns `audit_not_available` when strict audit is not ready
- [ ] Response size cap sets `truncated` without corrupting `has_more`
