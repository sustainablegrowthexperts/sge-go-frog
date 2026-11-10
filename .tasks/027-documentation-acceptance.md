---
id: task-027
title: "[Slice] Operators and AI clients have accurate artifact-download documentation and acceptance evidence"
status: completed
depends_on: ["task-026"]
target_files:
  - "README.md"
  - "DEPLOYMENT.md"
  - ".env.example"
  - "mcp_test.go"
---

## Objective

**Objective:** `README.md`, `DEPLOYMENT.md`, and the committed `.env.example` template document the absolute artifact URL shape, all seven artifact kinds, the `GET`/`HEAD` download workflow, completion polling, retention, and the configured-vs-request-derived base URL behavior, with a documentation contract test and the full acceptance suite proving no regressions.

## Layer-by-Layer Changes

### README

- Extend the existing MCP section with an agent-facing result-retrieval workflow:
  - `start_crawl` returns a job ID and absolute artifact links; poll `get_job` until the status is terminal.
  - After `done`, use `get_results`, `get_audit`, and `search_pages` for structured data.
  - Use `get_artifact` for metadata and an absolute URL, or download that URL directly.
  - HTML/text search requires `archive_html=true` or `audit=true`; audit retrieval requires `audit=true` on a spider job.
- Document the URL contract exactly:
  ```text
  <scheme>://<host><mcp-path>/artifacts/<job_id>/<kind>
  ```
  Include examples for CSV, audit ZIP, audit JSON, and audit XLSX under the default `/mcp` path.
- Document all seven kinds in one table:
  - `csv`
  - `audit_strict_zip`
  - `audit_inclusive_zip`
  - `audit_json_strict`
  - `audit_json_inclusive`
  - `audit_xlsx_strict`
  - `audit_xlsx_inclusive`
- Document download behavior:
  - `GET` returns the file; `HEAD` returns headers only; range requests are supported.
  - `Content-Type`, sanitized `Content-Disposition` filename, and `Content-Length` are set.
  - `404` for unknown/UI-owned/expired jobs, unknown kinds, or missing artifacts.
  - `409` for jobs that are not `done`.
  - `405` for unsupported methods.
  - `500` for generation failures with no internal details.
  - Downloads are restricted to MCP-created jobs; UI-created jobs are not reachable through MCP URLs.
  - URLs expire with the existing job retention window.
- Document base URL resolution:
  - `GOFROG_MCP_ARTIFACT_BASE_URL` sets the canonical public origin when configured.
  - When unset, the server derives the origin from the incoming MCP request (`X-Forwarded-Proto`/`X-Forwarded-Host`, then TLS/`Host`).
  - The configured MCP path is appended after the base; the base is not the full artifact URL.
- Preserve the existing web UI, CSV, audit, CLI, and `go-frog-audit` documentation; add links rather than duplicating large sections.

### DEPLOYMENT

- Add an "MCP artifact downloads" subsection covering:
  - The artifact route lives under the same MCP endpoint and listener; no new port, service, or volume is required.
  - The proxy must forward `GET` and `HEAD` for `<mcp-path>/artifacts/` to the same backend as `<mcp-path>`, and must preserve `X-Forwarded-Proto`/`X-Forwarded-Host` when the public origin differs from the container origin.
  - Set `GOFROG_MCP_ARTIFACT_BASE_URL` to the external canonical origin when proxy rewriting would otherwise produce wrong URLs; leave it empty to use request-derived URLs locally.
  - Downloads are limited to MCP-created, completed jobs; UI jobs remain browser-only.
  - Completed job artifacts disappear when the retention window expires and then return `404`.
  - Troubleshooting table for `404`, `409`, `405`, and `500` responses, including checking job status with `get_job` and confirming the job was created through MCP.
  - No app-level token handling, URL signing, or new secret is introduced; authentication stays at oauth2-proxy.
- Keep the existing results volume, restart behavior, and retention documentation accurate. Do not change deployment mechanics.
- Do not include real tokens, proxy credentials, or private hostnames.

### .env.example

- Update the `GOFROG_MCP_ARTIFACT_BASE_URL` comment to state:
  - It controls the canonical absolute origin for artifact links and downloads.
  - It should be the external proxy origin when configured.
  - When empty, the server derives the origin from the incoming request.
  - The MCP path is appended automatically, so the value should not include `/artifacts`.
- Keep the placeholder value on `example.com`; do not add secrets.
- **The developer manually creates `.env` from this template and fills in real values if needed. `.env` is gitignored and must never be read, copied from, or committed by this task.**

### Documentation Contract Test

- Extend `TestMCPDocumentationContract` in `mcp_test.go` with assertions that:
  - `README.md` contains the artifact URL shape, all seven kind names, `GET`, `HEAD`, `get_job` polling, `GOFROG_MCP_ARTIFACT_BASE_URL`, and the request-derived fallback.
  - `README.md` documents the `404`/`409`/`405`/`500` behavior and the MCP-ownership boundary.
  - `DEPLOYMENT.md` contains `<mcp-path>/artifacts/`, proxy forwarding for `GET`/`HEAD`, `X-Forwarded-Proto`, `X-Forwarded-Host`, retention expiry, and the troubleshooting statuses.
  - `.env.example` contains the canonical-base/request-derived-fallback comment and no real secret values.
  - The existing rejection list still forbids `cancel_job`, `stdio`, `gofrog://`, `multiple tokens`, and `horizontal scaling` in `README.md` and `DEPLOYMENT.md`.
- Use the existing `requireContractText` and `rejectContractText` helpers so the test fails with a readable missing-phrase message.

### Acceptance

- Run the full acceptance suite and record the result:
  ```bash
  go test ./...
  go vet ./...
  go test -race ./...
  ```
- If the workstation lacks a C compiler for `-race`, run the race suite in the documented container (`golang:1.26-alpine` with `gcc` and `musl-dev`) and record that environment, as the prior MCP acceptance did. Do not mark the task done on a race run that did not execute.
- Verify every in-scope item in `specs/2026-11-10-mcp-ai-result-access.md` has automated coverage or a documented manual check. Record any deliberate deviation in the spec.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- Extend `TestMCPDocumentationContract` with the README, DEPLOYMENT, and `.env.example` assertions described above.
- **Test execution command:**
  ```bash
  go test -run TestMCPDocumentationContract .
  ```
- **Expected result:** The test MUST fail because the documentation does not yet mention the artifact URL shape, all seven kinds, `GET`/`HEAD`, the base-URL fallback, the status mapping, or the proxy forwarding requirements. Confirm the failure is due to missing documentation, not a test typo.

### Step 2: GREEN Phase (Implementation)

- **Minimum documentation to write/modify:**
  - `README.md` — workflow, URL contract, seven kinds, download semantics, base URL resolution, retention.
  - `DEPLOYMENT.md` — route/proxy forwarding, canonical base URL, retention/expiry, troubleshooting.
  - `.env.example` — canonical-base and request-derived-fallback comment.
  - `mcp_test.go` — documentation contract assertions.
- **Test re-run command:**
  ```bash
  go test -run TestMCPDocumentationContract .
  ```
- **Expected result:** The test MUST pass. Required phrases are present, no real secrets or unsupported features are claimed, and existing documentation remains accurate.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Remove duplicated setup instructions between README and DEPLOYMENT; link instead where appropriate.
  - Verify every command and environment variable in the docs matches the implemented flags and names.
  - Verify the docs do not claim unsupported features (stdio, cancellation, resources, progress notifications, multiple tokens, horizontal scaling).
  - Confirm the `.env.example` comment matches the actual fallback behavior.
- **Refactor validation command:**
  ```bash
  go test -run TestMCPDocumentationContract . && go test ./...
  go vet ./...
  go test -race ./...
  ```
- **Expected result:** All tests, vet, and race checks pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing documentation
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] `go vet ./...` passes
- [ ] `go test -race ./...` passes, or the documented container-based equivalent is recorded
- [ ] README documents the URL shape, all seven kinds, `GET`/`HEAD`, polling, status mapping, ownership, and retention
- [ ] DEPLOYMENT documents proxy forwarding, canonical base URL, `X-Forwarded-*`, retention/expiry, and troubleshooting
- [ ] `.env.example` explains the configured-vs-request-derived base URL and contains no real secrets
- [ ] The documentation contract test guards the new content and rejects unsupported feature claims
- [ ] Every in-scope spec item has automated coverage or a documented manual check; deviations are recorded in the spec
