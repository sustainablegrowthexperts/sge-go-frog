---
id: task-021
title: "[Slice] MCP publishes a stable seven-tool schema contract"
status: pending
depends_on: ["task-020"]
target_files:
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp.go"
  - "mcp_test.go"
---

## Objective

**Objective:** The MCP server publishes exactly seven tools with stable names, descriptions, annotations, input schemas, and server metadata that clients can rely on across releases.

## Layer-by-Layer Changes

### Tool Catalog

- Ensure `mcp_tools.go` registers exactly these seven tools, in a deliberate order:
  1. `start_crawl`
  2. `get_job`
  3. `list_jobs`
  4. `get_results`
  5. `search_pages`
  6. `get_audit`
  7. `get_artifact`
- Add the MCP annotations from the spec:
  - `start_crawl`: `readOnlyHint=false`, `destructiveHint=false`, `idempotentHint=false`, `openWorldHint=true`.
  - Read tools: `readOnlyHint=true`, `idempotentHint=true`, `openWorldHint=false`.
- Do not register `cancel_job`, resources, prompts, sampling, or stdio tools in v1.
- Keep descriptions short and agent-oriented, including key constraints (job must be done, archive requirement for search, profile choice for audit).

### Schemas

- Define input schemas in `mcp_types.go` with the exact field names, types, enums, defaults, and required fields from the spec:
  - `start_crawl`: `mode`, `start_url`, `urls`, `keywords`, `concurrency`, `audit`, `audit_filters`, `archive_html`, `limits`, `client_request_id`.
  - `get_job`, `list_jobs`, `get_results`, `search_pages`, `get_audit`, `get_artifact` per their slices.
- Validate server-side even though the schema is a hint. The schema must not be the only validation.
- Keep additive fields backward-compatible; do not rename or repurpose existing fields.

### Server Metadata

- Include server name, version, and the MCP schema/tool version in `initialize` serverInfo and logs.
- Document the tested MCP protocol revision in the code comment or release notes.
- Keep `ArchiveVersion` and `AuditVersion` independent from the MCP schema version.

### Golden Contract Tests

- Add golden assertions in `mcp_test.go` for:
  - exact tool name set and count
  - annotations per tool
  - required/optional fields and enum values
  - serverInfo name/version shape
- A failure should tell the implementer exactly which schema field changed.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestToolSchemasGolden' .
  ```
- **Expected result:** The test MUST fail because the seven-tool catalog and golden schema assertions are not yet complete. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp_tools.go` — final seven-tool registration, descriptions, annotations.
  - `mcp_types.go` — final input schema definitions and constants.
  - `mcp.go` — server metadata/version.
- **Test re-run command:**
  ```bash
  go test -run 'TestToolSchemasGolden' .
  ```
- **Expected result:** Tests MUST pass. `tools/list` returns exactly the seven expected names with the correct annotations and schema fields; serverInfo includes name/version; no out-of-scope tool is registered.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Move schema literals next to their handlers or into a single catalog file for readability.
  - Ensure descriptions and annotations are not duplicated inconsistently.
  - Confirm the golden test is the only place that hard-codes the expected contract.
- **Refactor validation command:**
  ```bash
  go test -run 'TestToolSchemasGolden' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Exactly seven tools are registered
- [ ] Every tool has the correct annotations and schema fields
- [ ] Server metadata includes name/version and the schema/tool version
- [ ] No out-of-scope tool, resource, prompt, or stdio mode is exposed
