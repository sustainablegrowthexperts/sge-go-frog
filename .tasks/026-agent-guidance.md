---
id: task-026
title: "[Slice] AI discovers the start, poll, read, and download workflow from MCP guidance"
status: completed
depends_on: ["task-025"]
target_files:
  - "mcp.go"
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp_test.go"
---

## Objective

**Objective:** An MCP client receives concise server-level instructions and updated tool descriptions that explain the `start_crawl` → poll `get_job` → read results/audit/search → download artifact workflow, the archive/audit prerequisites, all seven artifact kinds, and how to use the absolute download URLs without outside documentation.

## Layer-by-Layer Changes

### Server Instructions

- In `mcp.go`, define a concise `mcpInstructions` constant covering:
  - What go-frog does: crawl public sites in spider or list mode and optionally audit internal linking.
  - The workflow: call `start_crawl`, then poll `get_job` until the status is terminal (`done`, `error`, or `cancelled`); do not assume results are immediately available.
  - What to use after completion:
    - `get_results` for filtered page rows and keyword counts.
    - `get_audit` for strict or inclusive audit sections.
    - `search_pages` for metadata, archived HTML, or visible text.
    - `get_artifact` or a direct `GET` of the returned absolute URL for files.
  - Prerequisites:
    - HTML/text search requires `archive_html=true` or `audit=true`.
    - Audit retrieval requires `audit=true` on a spider job.
    - Artifacts are available only when the job is `done`.
  - Artifact surface:
    - The seven kinds: `csv`, `audit_strict_zip`, `audit_inclusive_zip`, `audit_json_strict`, `audit_json_inclusive`, `audit_xlsx_strict`, and `audit_xlsx_inclusive`.
    - URLs are absolute, job-scoped, and under the configured MCP endpoint.
    - Downloads support `GET` and `HEAD`; URLs expire with the job retention window.
  - Error/limits summary for artifact downloads: `404` for unknown/expired, `409` for unfinished jobs, `405` for unsupported methods; limits and retention are server-enforced.
  - Do not advertise out-of-scope features: no `cancel_job`, no `gofrog://` resources, no progress notifications, no blocking wait tool, no stdio transport, no prompts/sampling/roots/elicitation.
- Pass the instructions to the SDK server:
  - `mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: mcpServerVersion}, &mcp.ServerOptions{Instructions: mcpInstructions})`
  - Keep the existing server name and version values unchanged.

### Tool Descriptions

- In `mcp_tools.go`, update descriptions so an agent can discover the workflow from `tools/list` alone:
  - `start_crawl`: accepts spider/list modes, keywords, audit, archive, filters, and limits; returns a job ID plus absolute artifact links; poll `get_job` for completion; state the audit/archive prerequisites.
  - `get_job`: the completion/polling mechanism; returns status, phase, progress, limits, audit state, error, and artifact readiness; list the terminal statuses.
  - `list_jobs`: unchanged except keep it consistent with MCP-created jobs only.
  - `get_results`: requires `done`; returns filtered, sorted, paginated page rows; optional strict-audit join.
  - `search_pages`: metadata search works without an archive; HTML/text search requires `archive_html=true` or `audit=true`; supports literal/RE2 regex and snippets.
  - `get_audit`: requires `audit=true`; supports strict and inclusive profiles; `links` is content-only and the ZIP has the full inventory.
  - `get_artifact`: returns an absolute job-scoped download URL, filename, content type, size, and expiry for a `done` job; lists all seven kinds; `GET`/`HEAD`; `inline=true` only for small CSV/audit JSON below the cap.
- Keep descriptions concise. Do not duplicate the full server instructions in every tool.
- Do not change annotations, tool names, input shapes, or output shapes in this task.

### Schema and Output Alignment

- In `mcp_types.go`:
  - Verify `getArtifactKinds` and the `getArtifactInput.Kind` jsonschema description list all seven kinds in the same order.
  - Verify `startCrawlLinks` includes `csv`, both audit ZIPs, both audit JSONs, and both audit XLSX links, all described as absolute.
  - Verify `getJobArtifacts` exposes CSV, strict/inclusive ZIP readiness, and strict/inclusive XLSX readiness.
- Do not add fields that the implementation does not populate.
- Keep the inline CSV/JSON behavior and the tool error contract unchanged.

### Scope Guardrails

- Do not add MCP resources, resource templates, prompts, sampling, progress notifications, cancellation, or a wait-until-done tool.
- Do not change authentication, retention, limits, or storage behavior.
- Do not mention implementation details that are not part of the agent-facing contract.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- Add `TestMCPWorkflowGuidance`:
  - Connect with `newMCPTestSession` and read `session.InitializeResult().Instructions`.
  - Assert the instructions are non-empty and contain the workflow markers: `start_crawl`, `get_job`, `get_artifact`, `get_results`, `get_audit`, `search_pages`, `done`, `archive_html`, `audit`, `GET`, `HEAD`.
  - Assert the instructions mention all seven artifact kinds.
  - Assert the instructions do not contain `cancel_job`, `gofrog://`, `stdio`, or progress-notification promises.
  - Call `tools/list` and assert:
    - `start_crawl` description mentions absolute artifact links and polling with `get_job`.
    - `get_job` description mentions polling/completion and terminal statuses.
    - `get_artifact` description mentions all seven kinds, an absolute URL, `GET`/`HEAD`, and that the job must be `done`.
    - `search_pages` mentions `archive_html` or `audit`; `get_audit` mentions `audit=true`.
  - Assert the existing golden tool schema still matches, especially the seven-kind `get_artifact.kind` enum.
- **Test execution command:**
  ```bash
  go test -run 'TestMCPWorkflowGuidance|TestToolSchemasGolden' .
  ```
- **Expected result:** The test MUST fail because the server currently passes `nil` server options (no instructions) and the tool descriptions do not mention the full artifact surface or workflow. Confirm the failure is due to missing guidance, not a test typo.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp.go` — add `mcpInstructions` and pass `&mcp.ServerOptions{Instructions: mcpInstructions}`.
  - `mcp_tools.go` — update the tool descriptions.
  - `mcp_types.go` — verify/update kind descriptions and link/readiness field comments.
  - `mcp_test.go` — add the workflow-guidance assertions and update any golden text expectations.
- **Test re-run command:**
  ```bash
  go test -run 'TestMCPWorkflowGuidance|TestToolSchemasGolden' .
  ```
- **Expected result:** Tests MUST pass. Instructions are discoverable at initialize time, descriptions reflect the working artifact surface, and the tool schema contract is unchanged.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Remove duplicated wording between server instructions and tool descriptions; keep the instructions as the workflow overview and the descriptions tool-specific.
  - Confirm no out-of-scope feature is advertised.
  - Verify every claim matches the implemented route, status codes, and retention behavior.
- **Refactor validation command:**
  ```bash
  go test -run 'TestMCPWorkflowGuidance|TestToolSchemasGolden' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing instructions/descriptions
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] `initialize` returns non-empty server instructions describing start → poll → read → download
- [ ] Instructions state the `archive_html`/`audit` prerequisites and the `done` requirement
- [ ] `get_artifact` description lists all seven kinds and explains absolute URLs plus `GET`/`HEAD`
- [ ] `get_job` is described as the completion/polling mechanism
- [ ] No out-of-scope feature is advertised
- [ ] Tool names, annotations, input schemas, and output shapes remain unchanged in this task
