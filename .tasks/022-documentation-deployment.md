---
id: task-022
title: "[Slice] Operators and clients have complete MCP documentation"
status: completed
depends_on: ["task-021"]
target_files:
  - "README.md"
  - "DEPLOYMENT.md"
  - "compose.yml"
  - ".env.example"
  - "mcp_test.go"
---

## Objective

**Objective:** Operators can configure, deploy, secure, restart, and troubleshoot the MCP endpoint, and MCP clients can configure Claude against it, with documentation and deployment files validated by a docs contract test.

## Layer-by-Layer Changes

### README

- Add an MCP section covering:
  - What MCP adds and the seven tools with one-line descriptions.
  - How to enable the endpoint (`GOFROG_MCP_HTTP`, `GOFROG_MCP_PATH`).
  - Authentication: oauth2-proxy with one shared bearer token; no app-level token handling.
  - Client configuration example for a remote Streamable HTTP MCP server (Claude).
  - Limits: pages, depth, list URLs, duration, concurrency, keywords, pattern length, URL length, request/response size, rate limit.
  - Retention: 24 hours after completion for MCP jobs; UI jobs unchanged.
  - Archive/search requirement: `audit=true` or `archive_html=true`.
  - Job lifecycle and polling with `get_job`.
  - Artifact access through authenticated URLs.
- Keep the existing UI, CSV, audit, CLI, and `go-frog-audit` documentation intact and accurate.

### DEPLOYMENT

- Document:
  - The `/results` volume and why it must persist across container recreation.
  - Proxy routing for `/mcp` (disable buffering, forward `Authorization` and `Mcp-Session-Id`, generous read timeout).
  - The existing proxy-facing port/path configuration.
  - Restart behavior: running MCP jobs are marked interrupted; completed jobs survive for the retention window.
  - Retention/cleanup behavior and disk usage expectations.
  - A restart/runbook procedure and troubleshooting checklist for MCP.
- Do not include real tokens, proxy credentials, or private hostnames.

### compose.yml

- Add the `/results` volume mount.
- Pass MCP enable/path, limits, TTL, results directory, archive default, and artifact base URL environment variables.
- Keep the existing port mapping and restart policy; do not expose an unauthenticated MCP listener.
- Use placeholders or environment lookups, never committed secrets.

### .env.example

- Keep the committed template with placeholder values only:
  - MCP enable/path
  - job TTL
  - results directory
  - server limits
  - archive default
  - public artifact base URL
- **The developer manually creates `.env` from the template and fills in real secrets. `.env` is gitignored and is never created, read, or committed by this task.**
- Authentication remains at oauth2-proxy; do not add an app-level token placeholder that implies the app validates it.

### Docs Contract Test

- Add `TestMCPDocumentationContract` in `mcp_test.go` that reads the documentation and deployment files and asserts:
  - README mentions all seven tool names.
  - README covers auth, limits, retention, and archive/search requirements.
  - DEPLOYMENT mentions the results volume, `/mcp`, proxy routing, restart behavior, and retention.
  - `compose.yml` has the results volume and MCP environment variables.
  - `.env.example` exists, uses placeholders, and contains no real-looking secret values.
- The test is a guard against documentation drift, not a substitute for the feature tests.

### Acceptance

- Run the full acceptance suite:
  ```bash
  go test ./...
  go test -race ./...
  go vet ./...
  ```
- Verify the Definition of Done items in `MCP.md` §23 and record any deliberate deviation in `specs/2026-11-10-go-frog-mcp-integration.md`.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestMCPDocumentationContract' .
  ```
- **Expected result:** The test MUST fail because the MCP documentation, compose volume/env, and `.env.example` content are missing or incomplete. Confirm the failure is due to missing documentation/config, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `README.md` — MCP setup, tools, client config, limits, retention, auth.
  - `DEPLOYMENT.md` — results volume, proxy routing, restart, retention, runbook.
  - `compose.yml` — results volume and MCP environment variables.
  - `.env.example` — placeholder template with no real secrets.
- **Test re-run command:**
  ```bash
  go test -run 'TestMCPDocumentationContract' .
  ```
- **Expected result:** Test MUST pass. All required sections and config keys are present, no real secret values are committed, and the existing documentation remains accurate.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Remove duplicated setup instructions between README and DEPLOYMENT; link instead where appropriate.
  - Verify every command in the docs is runnable and matches the actual flags/env names.
  - Confirm the docs do not claim unsupported features (stdio, cancel, resources, multi-token, horizontal scaling).
- **Refactor validation command:**
  ```bash
  go test -run 'TestMCPDocumentationContract' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [x] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [x] Implementation completed and tests re-run — all tests pass cleanly
- [x] Entire test suite passes without regressions (`go test ./...`)
- [x] `go test -race ./...` passes
- [x] `go vet ./...` passes
- [x] README documents the seven tools, auth, limits, retention, search requirements, and client setup
- [x] DEPLOYMENT documents the results volume, `/mcp` proxy routing, restart behavior, retention, and runbook
- [x] `compose.yml` and `.env.example` match the documented configuration and contain no real secrets
- [x] `MCP.md` §23 acceptance items are verified; deliberate deviations are recorded in the spec
