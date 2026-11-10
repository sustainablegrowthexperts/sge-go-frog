---
id: task-005
title: "[Slice] Agent starts list and audited spider jobs with options"
status: pending
depends_on: ["task-004"]
target_files:
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp_limits.go"
  - "server.go"
  - "mcp_test.go"
---

## Objective

**Objective:** An MCP client can start list-mode jobs and audited spider jobs with keywords, concurrency, audit filters, and per-job limits that are validated against server maxima.

## Layer-by-Layer Changes

### Data / Contracts

- Extend the `start_crawl` input DTO with:
  - `urls` for list mode
  - `audit` and `audit_filters` (`nav_substrings`, `exclude_substrings`, `explicit`)
  - `limits` (`max_pages`, `max_depth`, `max_duration_seconds`)
- Add server-limit configuration fields to `mcp_limits.go`: pages, depth, list URLs, duration, concurrency, keywords, response size, request size, search caps, URL length, and rate-limit values. Defaults come from the spec and are configurable.
- Per-job limits cannot raise server maxima. Store the effective limits on the job.

### Core Logic / Job Manager

- Reuse one job-launch path for UI and MCP that runs the crawler and then runs the strict/inclusive audit pipeline when enabled. Do not duplicate the audit orchestration in the MCP handler.
- List mode:
  - Deduplicate URLs.
  - Accept only HTTP(S) URLs.
  - Reject empty input and more than the configured URL cap.
  - Store `settings.listURLs` and set the target summary to `N URLs`.
- Spider mode with audit:
  - `audit=true` is spider-only; list mode plus audit returns `invalid_argument`.
  - Parse `audit_filters` only when `explicit=true`; otherwise ignore the object and use defaults + config. Do not merge half-explicit lists.
  - Use the existing `audit.Options.Filters` shape; do not add `force_content`/`force_exclude` schema fields.

### Limits / Validation

- `mcp_limits.go` validates:
  - `max_pages` 1..server max (default 5000)
  - `max_depth` 0..server max (default 10)
  - `max_duration_seconds` 1..server max (default 1800)
  - `concurrency` 1..server max (default 20)
  - list URL count <= 1000 after dedupe
  - keywords <= 20, each 1..100 characters, no `|`
  - URL length <= 2048
- Invalid values return `invalid_argument` naming the field.
- Limits above a server maximum are rejected, not silently clamped, so agents get deterministic behavior.
- `archive_html` is intentionally not part of this slice; task 011 adds it.

### MCP / Tool Layer

- Extend the `start_crawl` handler and schema for the new fields.
- Return the effective limits, mode, audit, archive flag, and job ID in the start response.
- Keep the response immediate; no blocking.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestStartCrawlOptions' .
  ```
- **Expected result:** The test MUST fail because list mode, audit filters, and per-job limit validation are not implemented. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp_tools.go` — extend `start_crawl`; add list-mode and audit validation.
  - `mcp_types.go` — options, filters, and effective-limit DTOs.
  - `mcp_limits.go` — server limits, defaults, and validation helpers.
  - `server.go` — shared job-launch path, list-mode settings, audit orchestration reuse.
- **Test re-run command:**
  ```bash
  go test -run 'TestStartCrawlOptions' .
  ```
- **Expected result:** Tests MUST pass. A list job starts with deduplicated URLs; a spider job with audit and explicit filters stores the filter profile; an audit list job fails; oversized URLs, too many keywords, and limits above maxima return `invalid_argument`.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract one `validateStartCrawl` function instead of scattering checks through the handler.
  - Share the audit-filter conversion with `parseAuditFilters` semantics where possible.
  - Confirm UI defaults and behavior are unchanged when MCP limits are absent.
- **Refactor validation command:**
  ```bash
  go test -run 'TestStartCrawlOptions' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] List mode and audited spider mode are both covered end-to-end
- [ ] Per-job limits cannot exceed server maxima and return `invalid_argument` when they do
- [ ] Explicit audit filters replace defaults; non-explicit objects are ignored
- [ ] UI crawl behavior remains unchanged
