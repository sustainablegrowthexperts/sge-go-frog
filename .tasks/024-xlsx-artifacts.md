---
id: task-024
title: "[Slice] AI retrieves standalone strict and inclusive audit XLSX workbooks"
status: completed
depends_on: ["task-023"]
target_files:
  - "internal/auditbundle/bundle.go"
  - "internal/auditbundle/bundle_test.go"
  - "mcp_artifacts.go"
  - "mcp_types.go"
  - "mcp_tools.go"
  - "server.go"
  - "mcp_test.go"
---

## Objective

**Objective:** A completed MCP audit job exposes `audit_xlsx_strict` and `audit_xlsx_inclusive` through `get_artifact`, returning a standalone XLSX built from the same `Summary`, `Pages`, `Links`, and `Blocks` workbook as the audit ZIP, with an absolute MCP-scoped URL, correct filename, content type, and size.

## Layer-by-Layer Changes

### Workbook Export

- In `internal/auditbundle/bundle.go`, expose the existing workbook generation path without duplicating sheet definitions:
  - Add `WriteWorkbook(srcDir, outPath string) error`.
  - Validate that `srcDir` exists and is a directory.
  - Reuse `readReportMeta`, `countRows`, and `buildWorkbook` exactly as the ZIP path does.
  - Write atomically: create a temporary file in the destination directory, write the workbook bytes, set `0o644`, and rename into place so a partial XLSX is never published.
  - Keep the existing `Workbook = "audit.xlsx"` constant and sheet layout unchanged.
  - If useful, extract a small `workbookBytes(srcDir string) ([]byte, error)` helper and call it from both `Build` and `WriteWorkbook`; do not fork the workbook logic.

### Artifact Model

- In `mcp_types.go`:
  - Append `audit_xlsx_strict` and `audit_xlsx_inclusive` to `getArtifactKinds` after the existing entries.
  - Update the `getArtifactInput.Kind` jsonschema description to list all seven kinds.
  - Add `AuditXLSXStrict` (`json:"audit_xlsx_strict"`) and `AuditXLSXInclusive` (`json:"audit_xlsx_inclusive"`) to `startCrawlLinks`; populate them through the absolute URL builder from task-023.
  - Add `AuditXLSXStrictReady` (`json:"audit_xlsx_strict_ready"`) and `AuditXLSXInclusiveReady` (`json:"audit_xlsx_inclusive_ready"`) to `getJobArtifacts`.
- In `server.go`, populate the two XLSX readiness fields from the strict/inclusive audit readiness in `statusSnapshot` so `get_job` exposes the same artifact surface as `get_artifact`.

### Artifact Resolution

- In `mcp_artifacts.go`:
  - Add `case "audit_xlsx_strict", "audit_xlsx_inclusive"` to `resolveArtifact`.
  - Resolve the profile, audit directory, and readiness with the existing `auditProfileState` helper.
  - If the audit is disabled, the profile is not ready, or the directory is missing, return the existing `audit_not_available` tool error.
  - Build the standalone workbook at `filepath.Join(s.jobOwnedDir(jobID), profile+"-audit.xlsx")` by calling `auditbundle.WriteWorkbook(cleanDir, path)`.
  - Cache per job, kind, and profile using the audit directory mtime, mirroring `auditBundlePath`. Rebuild when the mtime changes or the cached file is missing. A parallel cache is acceptable; do not break the existing ZIP cache.
  - Return an artifact descriptor with:
    - Filename `<slug>-<profile>-audit.xlsx`.
    - Content type `application/vnd.openxmlformats-officedocument.spreadsheetml.sheet`.
    - The absolute URL from task-023.
    - `inlineable: false`.
  - Extend `artifactProfileForLog` so the XLSX kinds log `strict` or `inclusive`.
  - Never include the audit directory or a filesystem path in a tool error or response.
- Preserve existing CSV, ZIP, JSON, inline, expiry, and error behavior.
- `get_artifact` with `inline=true` for an XLSX kind MUST return metadata with `inline=false` and no `content` value.

### Schema and Golden Contract

- Update `TestToolSchemasGolden` in `mcp_test.go` so the `get_artifact.kind` enum contains all seven kinds in the declared order.
- Update any `get_job` structured-output assertions that enumerate `artifacts` fields for the two new readiness flags.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test files to create/update:** `internal/auditbundle/bundle_test.go` and `mcp_test.go`
- Add tests:
  - `TestWriteWorkbook` in `internal/auditbundle/bundle_test.go`:
    - Build the fixture with `newFixture`, call `WriteWorkbook`, and open the result with `excelize`.
    - Assert the workbook contains the `Summary`, `Pages`, `Links`, and `Blocks` sheets.
    - Assert representative cells match the values in the workbook inside a `WriteFile` ZIP (same sheets, same rows), proving the standalone file and bundled workbook share one builder.
  - `TestGetArtifactXLSX` in `mcp_test.go`:
    - Create a done MCP audit job with strict and inclusive fixture directories (reuse `writeAuditBundleFixture` and `createDoneResultsJob`).
    - Call `get_artifact` with `kind: "audit_xlsx_strict"` and `kind: "audit_xlsx_inclusive"`, with and without `inline: true`.
    - Assert `filename` ends with `-strict-audit.xlsx` / `-inclusive-audit.xlsx`, `content_type` is the Excel MIME type, `size_bytes` is non-zero, and `url` is absolute under the MCP artifact path.
    - Assert `inline` is `false` and no `content` is present even when `inline: true`.
    - Open the generated file from the job-owned directory and verify the four sheets.
    - Assert a job without an audit returns `audit_not_available` for both XLSX kinds.
    - Assert `get_job.artifacts.audit_xlsx_strict_ready` and `audit_xlsx_inclusive_ready` track audit readiness.
- **Test execution command:**
  ```bash
  go test ./internal/auditbundle -run TestWriteWorkbook && go test -run 'TestGetArtifactXLSX|TestToolSchemasGolden' .
  ```
- **Expected result:** Tests MUST fail because `auditbundle.WriteWorkbook` does not exist and `get_artifact` rejects the XLSX kinds as invalid. Confirm the failure is due to missing workbook export and artifact-kind support, not a test typo.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `internal/auditbundle/bundle.go` — add the atomic `WriteWorkbook` export backed by the existing workbook builder.
  - `mcp_types.go` — add XLSX kinds, link fields, and readiness fields.
  - `server.go` — populate XLSX readiness in `statusSnapshot`.
  - `mcp_artifacts.go` — resolve, generate, cache, and describe standalone XLSX artifacts.
  - `mcp_tools.go` — only if the artifact summary or description needs the new kinds; keep the handler contract unchanged.
  - `mcp_test.go` — update the golden kind enum and add the XLSX assertions.
- **Test re-run command:**
  ```bash
  go test ./internal/auditbundle -run TestWriteWorkbook && go test -run 'TestGetArtifactXLSX|TestToolSchemasGolden' .
  ```
- **Expected result:** Tests MUST pass. Both XLSX kinds are generated for both profiles, the standalone workbook matches the ZIP workbook, metadata is correct, inline content is never returned for XLSX, and readiness fields are accurate.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Remove any duplicated workbook assembly; both the ZIP and standalone path must call the same builder.
  - Keep the XLSX cache key unambiguous across job, kind, and profile.
  - Verify no audit directory or filesystem path appears in errors, logs, or structured output.
  - Confirm CSV, ZIP, JSON, and inline behavior are untouched.
- **Refactor validation command:**
  ```bash
  go test ./internal/auditbundle -run TestWriteWorkbook && go test -run 'TestGetArtifactXLSX|TestToolSchemasGolden' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing XLSX export/kind support
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] `audit_xlsx_strict` and `audit_xlsx_inclusive` are accepted by `get_artifact`
- [ ] Standalone XLSX uses the same workbook builder and sheet/data as the audit ZIP
- [ ] Filename, content type, size, and absolute URL are correct for both profiles
- [ ] XLSX is never returned inline, even when `inline=true`
- [ ] `get_job.artifacts` reports XLSX readiness for both profiles
- [ ] The golden tool schema lists all seven artifact kinds
- [ ] No filesystem path or audit directory leaks in responses or errors
