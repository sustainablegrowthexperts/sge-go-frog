---
id: task-018
title: "[Slice] All MCP tools share a stable error and ownership contract"
status: pending
depends_on: ["task-017"]
target_files:
  - "mcp_tools.go"
  - "mcp_types.go"
  - "mcp.go"
  - "mcp_limits.go"
  - "mcp_test.go"
---

## Objective

**Objective:** Every MCP tool returns the stable `{code, message, retryable, details}` error contract, enforces the MCP-only ownership boundary, and rejects oversized requests/responses without leaking paths or tokens.

## Layer-by-Layer Changes

### Error Contract

- In `mcp_types.go`, define the error envelope and constants:
  - `invalid_argument`
  - `unauthorized`
  - `forbidden`
  - `job_not_found`
  - `job_not_finished`
  - `audit_not_available`
  - `search_not_available`
  - `limit_exceeded`
  - `conflict`
  - `internal_error`
- Provide `newToolError(code, message, details)` and a retryable mapping:
  - `limit_exceeded` is retryable.
  - `job_not_finished` and `internal_error` may be retryable depending on the detail; document the choice.
  - Validation, ownership, not-found, and conflict errors are not retryable.
- Return `isError: true` through the SDK adapter. Do not encode domain errors as successful content.

### Ownership Boundary

- Centralize job lookup for MCP tools:
  - Only `owner="mcp"` jobs are visible.
  - UI jobs and unknown IDs both return `job_not_found`, with no indication that a UI job exists.
  - A job ID from another owner never leaks settings, pages, audit data, or artifacts.
- Do not implement per-caller auth in the app; the shared token remains the proxy boundary. Ownership here means UI vs MCP job isolation, not multi-tenant isolation.

### Request / Response Caps

- Enforce a 1 MB request-body cap for MCP JSON-RPC requests. Oversized requests return `invalid_argument` or a protocol-level rejection, never a crash.
- Enforce the 5 MB response cap for tool results. Where a tool can paginate, stop at the cap and set `truncated`; otherwise return `limit_exceeded`.
- Cap URL length at 2,048 characters, pattern length at 512, keyword count/length, and audit/results limits per the earlier slices.
- Reject unknown JSON fields or malformed types with a field-named `invalid_argument` where the SDK allows it.

### Sanitization

- Never include in errors:
  - absolute or relative filesystem paths
  - audit/archive directory names
  - `Authorization` values, bearer tokens, cookies, or proxy credentials
  - stack traces or panic values
- Wrap handlers with panic recovery that returns `internal_error` with a generic message and logs the sanitized cause server-side.
- Log enough server-side context (job ID, tool, error code) without page bodies or secrets.

### MCP / Server

- Apply the wrapper to all seven tools uniformly.
- Ensure the MCP HTTP handler rejects malformed/oversized requests before tool execution.
- Keep the web UI error behavior unchanged.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestToolErrorContract' .
  ```
- **Expected result:** The test MUST fail because tool errors are not yet centralized and ownership/caps are inconsistent. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp_types.go` — error envelope, codes, retryable mapping.
  - `mcp_tools.go` — shared lookup/wrapper, panic recovery, all-tool adoption.
  - `mcp.go` — request-size rejection and protocol error mapping.
  - `mcp_limits.go` — response-size and input-cap helpers.
- **Test re-run command:**
  ```bash
  go test -run 'TestToolErrorContract' .
  ```
- **Expected result:** Tests MUST pass. Each error code is produced by the expected situation; UI jobs return `job_not_found`; running jobs return `job_not_finished`; missing audits/search archives return the specific not-available code; oversized requests are rejected; oversized responses are truncated or limited; no message contains a path or token.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Remove per-handler error construction in favor of one helper.
  - Use a shared `lookupMCPJob` function for every tool.
  - Add a table-driven test that walks every tool and asserts the same envelope shape.
- **Refactor validation command:**
  ```bash
  go test -run 'TestToolErrorContract' . && go test -race ./... && go vet ./...
  ```
- **Expected result:** All previously passing tests still pass under the race detector and vet. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] All ten error codes are mapped and covered by tests
- [ ] UI jobs are invisible to MCP tools and indistinguishable from unknown IDs
- [ ] Request and response caps are enforced
- [ ] Panics return `internal_error` and are logged without stack traces in the response
- [ ] No error or log leaks paths, tokens, cookies, or page bodies
