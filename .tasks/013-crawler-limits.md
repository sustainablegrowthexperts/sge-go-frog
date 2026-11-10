---
id: task-013
title: "[Slice] Crawlers stop at page, depth, duration, and URL caps"
status: pending
depends_on: ["task-012"]
target_files:
  - "spider.go"
  - "list.go"
  - "scrape.go"
  - "job_runner.go"
  - "server.go"
  - "audit_integration_test.go"
---

## Objective

**Objective:** Spider and list crawls stop at page, depth, duration, and list-URL caps, preserve partial results, and report `truncated=true` with a `limit_reason` instead of failing.

## Layer-by-Layer Changes

### Crawl Outcome / Contracts

- Extend the runner result to carry `pages`, `truncated`, and `limit_reason` in addition to `error`.
- Define limit reasons at least for `max_pages`, `max_depth`, `max_duration`, and `max_list_urls`.
- Keep the MCP start output and `get_job` DTOs compatible; they already expose `truncated` and limits from earlier slices.

### Spider Pipeline

- Add a context-aware entry point (`runSpiderContext` or equivalent) and keep the existing `runSpider` wrapper for the CLI.
- Apply `max_depth` to link following. Sitemap-coverage pages are outside the link frontier; treat them as depth 0 or apply a separate documented rule from the spec.
- Enforce `max_pages` against distinct recorded URLs, not progress callbacks. Include sitemap-only pages.
- Enforce `max_duration` over the whole crawl. Check the context/deadline before enqueueing new work and in callbacks; in-flight requests may finish up to the client timeout.
- When a cap is hit:
  - Stop scheduling new work.
  - Keep partial results.
  - Set `truncated=true` and the specific `limit_reason`.
  - Return successfully (`status=done`) rather than a hard error.
- If an audit archive is incomplete because of a cap, mark audit state `partial` or skip the audit; never present a partial archive as complete.

### List Pipeline

- Enforce the deduplicated list-URL cap before visiting.
- Apply the page and duration caps with the same semantics as spider mode.
- Preserve partial rows and truncation metadata.

### Recording

- Add a page-count limit to the shared recording path in `scrape.go` so concurrent callbacks cannot exceed the cap and race on the count.
- Record the first `limit_reason` deterministically when multiple caps are reached.
- Do not change CSV columns, page-row fields, or artifact shapes.

### Server / Job

- Pass effective limits from the job into the runner.
- Set `job.truncated` and `job.limitReason` from the runner outcome.
- UI and CLI runs without MCP limits keep their current unbounded behavior.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `audit_integration_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestCrawlerPageAndDepthLimits' .
  ```
- **Expected result:** The test MUST fail because the runners ignore limits and return only pages. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `spider.go` — context, depth/page/duration enforcement, partial results.
  - `list.go` — URL/page/duration enforcement.
  - `scrape.go` — concurrency-safe page counting and truncation.
  - `job_runner.go` — outcome struct and callbacks.
  - `server.go` — apply job limits and store truncation state.
- **Test re-run command:**
  ```bash
  go test -run 'TestCrawlerPageAndDepthLimits' .
  ```
- **Expected result:** Tests MUST pass. A site with many links stops at the page cap, respects depth, and reports the right reason; a list larger than the cap is rejected or truncated per the spec; a duration cap returns partial results; CSV output for a limited run keeps the existing columns.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract a small `crawlBudget` helper shared by spider and list.
  - Ensure the page count is incremented under one mutex and checked atomically with recording.
  - Confirm no test sleeps for the duration cap; use an injectable clock or very short deterministic deadline.
- **Refactor validation command:**
  ```bash
  go test -run 'TestCrawlerPageAndDepthLimits' . && go test -race ./...
  ```
- **Expected result:** All previously passing tests still pass under the race detector. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Page, depth, duration, and list-URL caps are enforced
- [ ] Partial results are preserved and `truncated`/`limit_reason` are set
- [ ] Concurrent recording cannot exceed the page cap or race
- [ ] UI/CLI runs without MCP limits are unchanged
- [ ] CSV columns and artifact shapes are unchanged
