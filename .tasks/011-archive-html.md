---
id: task-011
title: "[Slice] archive_html creates a searchable archive without audit"
status: pending
depends_on: ["task-010"]
target_files:
  - "main.go"
  - "spider.go"
  - "list.go"
  - "scrape.go"
  - "server.go"
  - "mcp_tools.go"
  - "audit_integration_test.go"
---

## Objective

**Objective:** `archive_html=true` creates a manifest and raw HTML archive in spider and list mode even when audit is off, and the archive is available for later result depth/discovery and content search.

## Layer-by-Layer Changes

### Settings / Contracts

- Add `ArchiveHTML bool` to `WizardSettings` in `main.go`.
- Add `archive_html` to the `start_crawl` input DTO and response. `archive_html=true` implies archive creation but not an audit report.
- `audit=true` also implies an archive; do not create two archives for one job.
- Store the effective archive flag and archive directory on the job.

### Crawl Pipeline

- `spider.go`:
  - Create an `auditpkg.ArchiveWriter` when `s.Audit || s.ArchiveHTML`.
  - Use the job-owned archive directory when provided; otherwise fall back to the existing run directory naming for the CLI.
  - Record every response, including non-200 metadata, with the existing `ArchiveWriter.Record`.
  - Keep sitemap discovery tied to `audit=true` for v1; `archive_html` alone does not fetch sitemaps.
  - Always close the archive writer and surface close errors; the manifest must be complete before the job is marked done.
- `list.go`:
  - Create and attach an archive writer when `s.ArchiveHTML || s.Audit`.
  - Record every visited URL; list mode has no link following.
  - Close the archive writer before returning.
- `scrape.go`:
  - Keep archive recording in the existing `OnScraped`/`OnError` paths; ensure depth is recorded for both successful and error responses.
  - Do not change page rows or CSV columns when an archive is enabled.

### Job / Server

- Allocate the archive directory under the job-owned directory (for example `results/jobs/<id>/archive`) before launching the runner.
- Pass the directory through `WizardSettings.AuditArchiveDir` (reuse the existing field) and set `job.archiveHTML`.
- Preserve partial archives if a crawl errors; mark audit state `partial` only when an audit was requested and the archive is incomplete.

### MCP / Tool Layer

- Extend `start_crawl` validation and response for `archive_html`.
- `get_results` depth/discovery from task 010 now works for jobs created with `archive_html`.
- Content search is implemented in task 016; this slice only guarantees the archive exists and is loadable.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `audit_integration_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestArchiveHTMLWithoutAudit' .
  ```
- **Expected result:** The test MUST fail because `archive_html` is not accepted and the runners only archive when audit is enabled. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `main.go` — `WizardSettings.ArchiveHTML`.
  - `spider.go` — archive when audit or archive_html is set; close and surface errors.
  - `list.go` — archive list-mode pages.
  - `scrape.go` — record depth/metadata without changing rows or CSV.
  - `server.go` — job-owned archive directory and job flag.
  - `mcp_tools.go` — accept and validate `archive_html`.
- **Test re-run command:**
  ```bash
  go test -run 'TestArchiveHTMLWithoutAudit' .
  ```
- **Expected result:** Tests MUST pass. A spider run and a list run with `archive_html=true` and `audit=false` produce `manifest.json` and HTML bodies; no `audit.json` is created; `get_results` exposes depth/discovery; the CSV columns are unchanged.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract archive setup/close into helpers shared by spider and list.
  - Verify the archive is closed exactly once and no `.tmp` manifest remains.
  - Confirm audit and archive-only runs do not duplicate manifest writes.
- **Refactor validation command:**
  ```bash
  go test -run 'TestArchiveHTMLWithoutAudit' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Spider and list mode both archive with `archive_html=true` and no audit
- [ ] `manifest.json` is complete and `audit.json` is absent for archive-only runs
- [ ] CSV columns and page-row behavior are unchanged
- [ ] `get_results` exposes depth/discovery for archive-enabled jobs
