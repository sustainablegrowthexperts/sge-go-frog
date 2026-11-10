---
id: task-014
title: "[Slice] get_job shows live crawl and audit phases"
status: completed
depends_on: ["task-013"]
target_files:
  - "spider.go"
  - "list.go"
  - "server.go"
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp_test.go"
---

## Objective

**Objective:** An MCP client polling `get_job` sees live phase transitions from `starting` through `crawling`, sitemap coverage, strict/inclusive auditing, and `done` or `error`.

## Layer-by-Layer Changes

### Phase Model

- Keep the phase constants from the spec: `queued`, `starting`, `discovering_sitemap`, `crawling`, `sitemap_coverage`, `auditing_strict`, `auditing_inclusive`, `done`, `error`, `cancelled`.
- Add a thread-safe `setPhase(phase)` method on `crawlJob` that:
  - Sets `startedAt` on the first active phase if not already set.
  - Sets `finishedAt` on `done`/`error`/`cancelled`.
  - Persists the phase transition through the job store when persistence is enabled.
  - Never regresses from a terminal phase back to an active phase.

### Runner Callbacks

- Extend the runner callback surface with an `onPhase` callback alongside `onProgress`.
- `spider.go`:
  - `starting` before URL normalization/archive setup.
  - `discovering_sitemap` before sitemap discovery when audit is enabled.
  - `crawling` before the first page visit.
  - `sitemap_coverage` before fetching sitemap-only pages.
- `list.go`:
  - `starting`, then `crawling`; no sitemap or audit phases.
- The fake runner in tests must be able to emit each phase deterministically.

### Job Orchestration

- In `server.go`:
  - Set `starting` when the job goroutine actually begins.
  - Set `auditing_strict` before the strict audit and `auditing_inclusive` before the inclusive audit.
  - Set `done` after all audits and artifact readiness updates.
  - Set `error` on crawl or unrecoverable orchestration errors.
- Audit failures remain separate: the job can be `done` with `audit.state=error` or `partial`.
- Update audit state (`pending`, `running`, `ready`, `error`, `partial`) at the same transitions.

### MCP / Tool Layer

- `get_job` returns the current phase and audit state from the job snapshot.
- Do not change the start response phase semantics: it may return `starting` or `queued`.
- Keep the HTML UI's `status == "running"` treatment unchanged; phase is additive.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestJobPhases' .
  ```
- **Expected result:** The test MUST fail because the runner emits no phases and the job does not track phase transitions. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `server.go` — phase transition method, audit phase orchestration, persistence hook.
  - `spider.go` — phase callbacks around discovery, crawling, and sitemap coverage.
  - `list.go` — starting/crawling callbacks.
  - `mcp_tools.go` — expose phase and audit state in `get_job`.
  - `mcp_types.go` — phase/audit-state constants or mappings.
- **Test re-run command:**
  ```bash
  go test -run 'TestJobPhases' .
  ```
- **Expected result:** Tests MUST pass. A fake runner drives the job through `starting` → `crawling` → `auditing_strict` → `auditing_inclusive` → `done`; list mode skips sitemap/audit phases; an error ends in `error`; `finishedAt` is set only at terminal phases.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Centralize phase transitions in the job, not in multiple callbacks.
  - Ensure phase updates and progress updates do not deadlock the job mutex.
  - Verify persistence writes do not happen on every page progress event.
- **Refactor validation command:**
  ```bash
  go test -run 'TestJobPhases' . && go test -race ./...
  ```
- **Expected result:** All previously passing tests still pass under the race detector. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Spider mode exposes sitemap and audit phases; list mode skips them
- [ ] `startedAt` and `finishedAt` follow the spec lifecycle
- [ ] Audit failures do not turn the crawl into a phase error
- [ ] Existing UI status rendering is unchanged
