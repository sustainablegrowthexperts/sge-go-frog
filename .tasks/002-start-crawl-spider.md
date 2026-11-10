---
id: task-002
title: "[Slice] Agent starts a spider crawl and receives a job ID"
status: completed
depends_on: ["task-001"]
target_files:
  - "server.go"
  - "job_runner.go"
  - "mcp.go"
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp_test.go"
  - "server_test.go"
---

## Objective

**Objective:** An MCP client can call `start_crawl` for a spider job and immediately receive a running MCP-owned job ID, phase, and polling hint without blocking on the crawl.

## Layer-by-Layer Changes

### Data / Contracts

- Create `mcp_types.go` with the first version of the tool DTOs:
  - `start_crawl` input: `mode`, `start_url`, `keywords`, `concurrency`.
  - `start_crawl` output: `job_id`, `status`, `phase`, `poll_after_ms`, `audit`, `archive_html`, and `links`.
  - Phase/status constants from the spec (`starting`, `crawling`, `done`, `error`; `running`, `done`, `error`).
- Extend `crawlJob` in `server.go` with: `phase`, `startedAt`, `finishedAt`, `owner`, `limits`, `truncated`, `limitReason`, `archiveHTML`, and `clientRequestID`. Keep `jobView` and all HTML fragment fields unchanged.
- Add a 128-bit MCP job ID generator (`newMCPJobID`) using `crypto/rand`; leave the existing UI `newJobID` shape untouched.

### Core Logic / Job Manager

- Extract shared app/job-manager construction from `startServer` so the web UI and MCP use the same manager and crawl pipeline.
- Add job-manager operations required by this slice:
  - `createWithOwner(owner, mode, target, settings)` — UI jobs use `owner="ui"`, MCP jobs use `owner="mcp"`.
  - `list(filter)` — returns MCP-visible jobs only; used by later slices.
  - `runningCount(owner)` — current running count.
  - `delete(id)` — safe removal for later retention work.
- Add job phase transition helpers that set `startedAt` on the first active phase and `finishedAt` on terminal status.
- Keep the existing `/api/crawl` behavior and job fragment polling intact; route it through `createWithOwner("ui", ...)`.

### Crawl Runner / Process

- Create `job_runner.go`:
  - `crawlRunner` interface with `Spider(ctx, settings, onProgress)` and `List(ctx, urls, settings, onProgress)`.
  - Real implementation wrapping `runSpider`/`runList` (context is threaded now even if the crawlers only check it at boundaries in this slice).
  - Controllable fake runner for tests: accepts a start signal, emits progress, blocks until released, and returns fixed pages/errors.
- Do not change CSV columns, page-row fields, or audit artifact shapes.

### MCP / Tool Layer

- Register `start_crawl` in `mcp_tools.go` using the SDK adapter.
- Validate spider input:
  - `mode == "spider"`.
  - `start_url` is present, normalized with `normalizeHTTPURL`, and HTTP(S).
  - `keywords` may be empty; each keyword is non-empty and contains no `|`.
  - `concurrency` defaults to 10 and must be between 1 and the configured server maximum.
- Create a job with owner `mcp`, set phase `starting`, launch the runner asynchronously, and return `job_id`, `status="running"`, `phase`, `poll_after_ms`, and artifact link placeholders.
- A crawl that finishes immediately must set phase `done`; a crawl error must set phase `error` and store the error for `get_job`.

### API / Server

- Mount the tool handler through the MCP server from `mcp.go`.
- Do not add per-job limits, list mode, audit options, `archive_html`, or idempotency here; later slices add them.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go` and `server_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestStartCrawlSpider|TestUIJobFlowUnchanged' .
  ```
- **Expected result:** `TestStartCrawlSpider` MUST fail because `start_crawl`, the runner seam, and the MCP-owned job model do not exist. `TestUIJobFlowUnchanged` MUST also fail if the shared-core extraction breaks the current UI contract. Confirm failures are due to missing behavior, not unrelated syntax/import errors.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `server.go` — shared app core, job model fields, manager operations, UI owner.
  - `job_runner.go` — `crawlRunner`, real adapter, fake runner.
  - `mcp.go` — register the tool through the adapter.
  - `mcp_tools.go` — `start_crawl` handler and spider validation.
  - `mcp_types.go` — start-crawl DTOs and phase/status constants.
- **Test re-run command:**
  ```bash
  go test -run 'TestStartCrawlSpider|TestUIJobFlowUnchanged' .
  ```
- **Expected result:** Tests MUST pass. The MCP response has a 32-hex-character job ID, `status=running`, a phase, and a positive `poll_after_ms`; the manager contains one `owner=mcp` job; the UI still renders a job fragment.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Remove duplicated job-creation logic between the UI and MCP paths.
  - Ensure `jobView` stays a browser-only projection and the MCP DTO stays separate.
  - Confirm no UI HTML fragment fields changed.
- **Refactor validation command:**
  ```bash
  go test -run 'TestStartCrawlSpider|TestUIJobFlowUnchanged' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] `go test -race ./...` passes for the shared job manager
- [ ] `start_crawl` returns immediately; it never blocks for the crawl
- [ ] MCP-created jobs use 128-bit IDs and `owner="mcp"`; UI jobs keep their current ID shape and `owner="ui"`
- [ ] Existing UI crawl flow and job fragment output are unchanged
