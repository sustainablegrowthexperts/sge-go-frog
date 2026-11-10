---
id: task-001
title: "[Slice] MCP client initializes over /mcp"
status: completed
depends_on: []
target_files:
  - "go.mod"
  - "go.sum"
  - "main.go"
  - "server.go"
  - "mcp.go"
  - "mcp_test.go"
  - ".env.example"
  - "compose.yml"
---

## Objective

**Objective:** An MCP client can connect to `/mcp` over Streamable HTTP, complete `initialize`, and receive go-frog server metadata while the existing web UI starts and behaves unchanged.

## Layer-by-Layer Changes

### Data / Contracts

- Pin the official Go SDK `github.com/modelcontextprotocol/go-sdk` at an explicit version in `go.mod`; run `go mod tidy` so `go.sum` records the checksums.
- Create `mcp.go` with a small adapter around the SDK: construction, server name/version metadata, the Streamable HTTP handler, and a `Close(ctx)` lifecycle hook. Keep SDK-specific types behind this adapter so later tools do not leak protocol types into the job manager.
- Do not implement authentication in the app. The deployment's oauth2-proxy remains the auth boundary; never read or log the `Authorization` header in this slice.

### Core Logic / Config

- Add an `MCPConfig` (or equivalent) in `main.go` with: enable flag, HTTP path, job TTL, results directory, server limits, default archive behavior, and artifact base URL.
- Parse standard flags and matching environment variables. Preserve the existing `--cli` and `--addr` behavior and the manual-CLI path. MCP must be off by default; when disabled, `/mcp` must not be mounted.
- Add a `runServer`/startup path that returns errors instead of discarding them, so a failed MCP mount or listener is visible.

### API / Server

- Extract the minimum shared app/mux construction out of `startServer` so the existing routes and the MCP handler share one listener and one job manager. Do not change the HTML/HTMX response shapes.
- Mount the SDK's Streamable HTTP handler at the configured path (`/mcp` by default) only when MCP is enabled. The handler must be reachable on the existing listener, not a second listener.
- Register no tools yet. A `tools/list` result with an empty tool set is acceptable for this slice; the handshake capability is the deliverable.

### Config / Deployment

- Create `.env.example` with placeholder values only:
  - MCP enable/path
  - job TTL
  - results directory
  - server limits
  - archive default
  - public artifact base URL
- **The developer manually creates `.env` from the template and fills in real secrets. `.env` is gitignored and is never created, read, or committed by this task.**
- Update `compose.yml` to mount a `/results` volume and pass the MCP environment variables through. Keep the existing proxy-facing port mapping and do not add app-level token validation.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestMCPInitializeHandshake' .
  ```
- **Expected result:** The test MUST fail because the MCP server construction, config, and `/mcp` handler do not exist yet. Confirm the failure names the missing MCP behavior (undefined symbol or failed assertion), not an unrelated syntax/import error. The test must also assert that an MCP-disabled app does not serve `/mcp` and that `/` still renders the UI.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `go.mod`, `go.sum` — add and pin the official MCP Go SDK.
  - `main.go` — MCP config, flags, environment fallback, startup path.
  - `server.go` — shared app/mux construction and conditional `/mcp` mount.
  - `mcp.go` — SDK adapter, server metadata, Streamable HTTP handler, lifecycle hook.
  - `.env.example` — committed template with placeholders.
  - `compose.yml` — `/results` volume and MCP environment variables.
- **Test re-run command:**
  ```bash
  go test -run 'TestMCPInitializeHandshake' .
  ```
- **Expected result:** Test MUST pass cleanly. The SDK client initializes, receives the expected server name/version, and the existing `/` route still responds.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Remove duplicated flag/env parsing; use one config struct and one defaulting function.
  - Verify no token, cookie, or `Authorization` value is logged.
  - Confirm MCP-disabled startup has exactly the previous routing behavior.
- **Refactor validation command:**
  ```bash
  go test -run 'TestMCPInitializeHandshake' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] `go vet ./...` passes
- [ ] MCP is disabled by default; enabling it mounts `/mcp` on the existing listener
- [ ] `.env.example` is committed and contains no real secrets; `.env` is not committed or required for tests
- [ ] Existing web UI routes and HTML responses are unchanged
