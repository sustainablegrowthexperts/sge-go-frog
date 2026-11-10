---
id: task-019
title: "[Slice] MCP tool calls and job events emit structured logs"
status: completed
depends_on: ["task-018"]
target_files:
  - "mcp.go"
  - "mcp_tools.go"
  - "server.go"
  - "mcp_limits.go"
  - "mcp_test.go"
  - "server_test.go"
---

## Objective

**Objective:** Tool calls and job lifecycle events emit structured `log/slog` records with tool, job, target host, outcome, page/byte counts, and error code, while never logging tokens, page bodies, or proxy credentials.

## Layer-by-Layer Changes

### Logging Setup

- Configure `log/slog` in `main.go`/`mcp.go`:
  - Default `text` handler; `GOFROG_LOG_FORMAT=json` selects JSON.
  - Include a stable service name/version.
  - Inject the logger into the shared app and MCP adapter; do not use package-global loggers in handlers.
- Do not log the `Authorization` header, cookies, bearer tokens, proxy credentials, or full request bodies.

### Tool-Call Logging

- Log one structured record per tool call:
  - `tool`
  - `job_id` when available
  - `target_host` (host only, not full URL with query)
  - `outcome` (`ok`, `error`)
  - `error_code` when applicable
  - `duration_ms`
  - `pages` / `bytes` for results and artifacts where available
- Do not log raw patterns for `search_pages` at info level if they could contain sensitive text; log pattern length or a hash instead.
- Do not log artifact inline content or page bodies.

### Job Lifecycle Logging

- Log:
  - job creation with owner, mode, target host, limits
  - phase transitions with job ID and phase
  - completion with pages, duration, truncated/limit reason
  - audit state changes and artifact builds with size/profile
  - limit/rate events with the limit name and retry hint
  - auth failures where the proxy exposes them, without credentials
- Use consistent field names across tools and server events so operators can query them.

### Error Logging

- Log sanitized error causes server-side; never include filesystem paths, tokens, page bodies, or stack traces in MCP responses.
- Panic recovery logs the panic value and stack server-side but returns only `internal_error` to the client.

### Tests

- Use `slog.NewJSONHandler` with a buffer in tests.
- Assert required fields are present and values are correct.
- Assert the bearer token and a synthetic page body string are absent from the log output.
- Assert `Authorization` is not logged.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go` and `server_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestStructuredLogging' .
  ```
- **Expected result:** The test MUST fail because tool/lifecycle logging does not exist or is unstructured. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp.go` — logger construction/injection and server lifecycle logs.
  - `mcp_tools.go` — tool-call start/end/error logs.
  - `server.go` — job creation, phase, completion, audit, and artifact logs.
  - `mcp_limits.go` — limit/rate event logs.
- **Test re-run command:**
  ```bash
  go test -run 'TestStructuredLogging' .
  ```
- **Expected result:** Tests MUST pass. A tool call emits the required fields; a job lifecycle emits create/phase/finish records; limit events include the limit name; token, page-body, and `Authorization` values never appear.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Introduce small logging helpers (`logToolCall`, `logJobEvent`) instead of repeating key-value pairs.
  - Verify field names match the spec and are consistent between text and JSON handlers.
  - Confirm log volume is bounded (no per-page info logs by default).
- **Refactor validation command:**
  ```bash
  go test -run 'TestStructuredLogging' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Tool calls log tool, job, target host, outcome, duration, and error code
- [ ] Job lifecycle logs create, phase, complete, audit, artifact, and limit events
- [ ] Text and JSON log formats both work
- [ ] Tokens, cookies, `Authorization`, page bodies, and proxy credentials are never logged
- [ ] Error logs are sanitized and do not expose filesystem paths
