---
id: task-017
title: "[Slice] Agent downloads CSV, audit ZIPs, and audit JSON"
status: completed
depends_on: ["task-016"]
target_files:
  - "mcp_artifacts.go"
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp_test.go"
---

## Objective

**Objective:** An MCP client can obtain CSV, strict/inclusive audit ZIPs, and strict/inclusive audit JSON metadata with an authenticated URL and optional inline content below the size cap.

## Layer-by-Layer Changes

### Artifact Contracts

- Add `get_artifact` DTOs to `mcp_types.go`:
  - Input: `job_id`, `kind`, `inline`.
  - Output: `job_id`, `kind`, `filename`, `content_type`, `size_bytes`, `url`, `expires_at`, `inline`, and optional `content`.
- Kinds:
  - `csv`
  - `audit_strict_zip`
  - `audit_inclusive_zip`
  - `audit_json_strict`
  - `audit_json_inclusive`
- Reject any other kind and any path-like input with `invalid_argument`.

### Artifact Builder

- Create `mcp_artifacts.go`:
  - Require `status == "done"`; otherwise `job_not_finished`.
  - CSV is generated at request time from the job's `[]Page` using the existing `writeResultsCSV`, so columns remain unchanged. Write it under the job-owned directory with an atomic file write.
  - Audit ZIPs are built with `auditbundle.WriteFile` from the job's strict/inclusive audit directory. Cache per job/profile when repeated requests are expected; invalidate if the audit directory mtime changes.
  - Audit JSON returns metadata for `audit.json`; when `inline=true`, return the file content only if it is below the configured inline cap (default 1 MB).
  - `filename` uses the existing result naming/slug rules for CSV and `<slug>-<profile>-audit.zip` for bundles.
  - `content_type` is `text/csv`, `application/zip`, or `application/json`.
  - `size_bytes` is the exact artifact size.
  - `expires_at` is based on the job's retention completion time (RFC 3339 UTC).
- Authenticated URL:
  - Build from the configured public artifact base URL plus a job-scoped path (for example `/api/crawl/<id>/download` or `/api/crawl/<id>/audit/bundle.zip`).
  - The URL must remain behind the deployment's oauth2-proxy. Do not add an unauthenticated MCP artifact path.
  - Do not accept or echo arbitrary filesystem paths.
- Inline cap:
  - If `inline=true` and the artifact exceeds the cap, return metadata with `inline=false` and no content; do not error.
  - If the artifact is missing or the audit profile is not ready, return `audit_not_available`.
- Do not expose `auditDir`, `archiveDir`, or any absolute path in the response.

### MCP / Tool Layer

- Register `get_artifact` in `mcp_tools.go`.
- Validate `kind` and `inline`; return structured content plus a short text summary with filename and size.
- Ensure the handler cannot be used to read files outside the job directory.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestGetArtifact' .
  ```
- **Expected result:** The test MUST fail because `get_artifact` and the artifact builder do not exist. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp_artifacts.go` — CSV generation, ZIP bundling, audit JSON metadata, inline cap, authenticated URL, cache.
  - `mcp_tools.go` — register and implement `get_artifact`.
  - `mcp_types.go` — artifact input/output DTOs.
- **Test re-run command:**
  ```bash
  go test -run 'TestGetArtifact' .
  ```
- **Expected result:** Tests MUST pass. CSV metadata and inline content are correct; strict/inclusive ZIPs open as valid ZIPs with the expected entries; audit JSON metadata and inline below/above cap work; invalid kinds and paths are rejected; unfinished jobs return `job_not_finished`; missing audits return `audit_not_available`; URLs include the job ID and never a filesystem path.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract artifact path resolution and size checks into helpers.
  - Ensure cached ZIPs are invalidated by audit directory mtime.
  - Verify no path or token is logged or returned.
- **Refactor validation command:**
  ```bash
  go test -run 'TestGetArtifact' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] All five artifact kinds are covered
- [ ] CSV columns are unchanged and generated at request time
- [ ] ZIPs are built with `auditbundle.WriteFile` and open correctly
- [ ] Inline content respects the 1 MB cap and falls back to metadata
- [ ] Artifact URLs are authenticated and job-scoped; arbitrary paths are rejected
