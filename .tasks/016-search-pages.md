---
id: task-016
title: "[Slice] Agent searches crawl metadata, HTML, and visible text"
status: pending
depends_on: ["task-015"]
target_files:
  - "mcp_search.go"
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp_test.go"
  - "audit_integration_test.go"
---

## Objective

**Objective:** An MCP client can search crawled page metadata, raw HTML, or visible text with literal or RE2 regex patterns and receive counted matches, snippets, pagination, and truncation.

## Layer-by-Layer Changes

### Search Contracts

- Add `search_pages` DTOs to `mcp_types.go`:
  - Input: `job_id`, `pattern`, `regex`, `case_sensitive`, `scope` (`metadata`, `html`, `text`, `all`), `context_chars`, `offset`, `limit`, `max_matches_per_page`.
  - Output: `job_id`, `pattern`, `regex`, `scope`, `total_matches`, `offset`, `limit`, `has_more`, `truncated`, and `matches`.
  - Match: `url`, `field`, `count`, `snippets` (`before`, `match`, `after`).
- Defaults: literal search, case-insensitive; `context_chars=120`, `limit=50`; max `context_chars=500`, max `limit=200`.

### Search Engine

- Create `mcp_search.go`:
  - `metadata` scope searches URL, title, description, H1s, robots, and serialized inlinks from `[]Page`. It works without an archive.
  - `html` scope requires an archive; read the raw HTML body from the manifest entry.
  - `text` scope requires an archive; strip tags with goquery before searching.
  - `all` combines metadata and the available archive scopes.
  - If `html`/`text`/`all` needs an archive and the job has none, return `search_not_available` with a hint to use `audit=true` or `archive_html=true`.
- Literal search:
  - Case-insensitive by default using a Unicode-aware lowercase comparison.
  - Count every non-overlapping occurrence per field/page.
  - Snippets include up to `context_chars` before and after each match; truncate safely on UTF-8 boundaries.
- Regex search:
  - Use Go `regexp` (RE2), `regexp.Compile`.
  - Cap pattern length at 512 characters.
  - Invalid regex returns `invalid_argument`.
  - Cap matches per page and total matches; never allow catastrophic work.
- Worker pool:
  - Scan manifest entries with a bounded pool (for example 8 workers).
  - Stop at the configured total-match cap and set `truncated=true`.
  - Preserve deterministic ordering by URL then field then match offset.
- Pagination is by match group (page/field), not by page row; a single page may contribute multiple matches across fields.

### MCP / Tool Layer

- Register `search_pages` in `mcp_tools.go`.
- Require a completed job; running jobs return `job_not_finished`.
- Validate `scope` values, bounds, and `max_matches_per_page` (positive, capped).
- Cap the serialized response size and set `truncated` instead of returning an unbounded payload.

### Archive Interaction

- Use `auditpkg.LoadArchive` and `Archive.ReadBody`; preserve path-traversal protections.
- Do not re-fetch pages or run a new crawl during search.
- Do not log page bodies or matched secret content.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go` and `audit_integration_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestSearchPages' .
  ```
- **Expected result:** The test MUST fail because `search_pages` and the search engine do not exist. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp_search.go` — scopes, literal/regex search, snippets, worker pool, caps.
  - `mcp_tools.go` — register and implement `search_pages`.
  - `mcp_types.go` — search input/output and match DTOs.
- **Test re-run command:**
  ```bash
  go test -run 'TestSearchPages' .
  ```
- **Expected result:** Tests MUST pass. Metadata search works without an archive; HTML/text search works with an archive; case-sensitive and case-insensitive literal search differ correctly; regex works and invalid regex fails; snippets are correct; pagination and truncation work; no-archive HTML/text returns `search_not_available`.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract matching and snippet construction into pure helpers.
  - Ensure the worker pool does not leak goroutines on cancellation or early stop.
  - Verify text extraction handles malformed HTML without panicking.
- **Refactor validation command:**
  ```bash
  go test -run 'TestSearchPages' . && go test -race ./...
  ```
- **Expected result:** All previously passing tests still pass under the race detector. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Metadata search works without an archive
- [ ] HTML and text search work with an archive and return `search_not_available` without one
- [ ] Literal and RE2 regex search, case sensitivity, snippets, and UTF-8 safety are covered
- [ ] Pattern, match, snippet, and response caps are enforced with `truncated`
- [ ] Search never re-fetches pages or leaks page bodies into logs/errors
