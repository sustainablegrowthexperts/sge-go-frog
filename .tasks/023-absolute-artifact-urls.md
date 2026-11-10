---
id: task-023
title: "[Slice] Absolute MCP-scoped artifact URLs replace relative links and the dead self link"
status: completed
depends_on: ["task-022"]
target_files:
  - "server.go"
  - "mcp.go"
  - "mcp_types.go"
  - "mcp_artifacts.go"
  - "mcp_tools.go"
  - "mcp_test.go"
  - "server_test.go"
---

## Objective

**Objective:** Every artifact link returned by `start_crawl` and every `get_artifact.url` is an absolute URL of the form `<scheme>://<host><mcp-path>/artifacts/<job_id>/<kind>`, using `GOFROG_MCP_ARTIFACT_BASE_URL` when configured and the incoming MCP request otherwise, and `start_crawl` no longer advertises the unreadable `gofrog://` self link.

## Layer-by-Layer Changes

### Request-Scoped Base URL

- Add a request-scoped base URL resolver shared by every URL-producing handler:
  - Prefer the trimmed, trailing-slash-stripped `MCPConfig.ArtifactBaseURL` (`GOFROG_MCP_ARTIFACT_BASE_URL`).
  - When it is empty, derive the origin from the incoming MCP HTTP request and store it in the request context before the SDK handles the call.
  - Scheme precedence: first `X-Forwarded-Proto` value, otherwise `https` when `r.TLS != nil`, otherwise `http`.
  - Host precedence: first `X-Forwarded-Host` value, otherwise `r.Host`.
  - Ignore untrusted query/body values; URLs come only from configuration or transport metadata.
- Put the derivation middleware beside the application wiring in `server.go` and the context-key/accessor in `mcp.go` (or the reverse, as long as both files change). In `newApp`, wrap the mounted MCP handler with it:
  - `mux.Handle(cfg.MCP.Path, withArtifactBase(ms.Handler()))`
  - Keep the exact MCP endpoint path unchanged.
- Normalize the MCP path once: use `defaultMCPPath` when empty, require a leading `/`, and trim trailing `/` so `/mcp/` and `/mcp` produce the same artifact prefix.
- If neither the configured base nor request context is available, return an empty URL rather than a bare relative path. This state is unreachable through `newApp`; do not fabricate a host.

### Artifact URL Model

- In `mcp_types.go`:
  - Remove the `Self` field (`json:"self"`) from `startCrawlLinks` and stop producing `gofrog://` URIs anywhere.
  - Keep the existing JSON field names for `csv`, `audit_strict_zip`, and `audit_inclusive_zip`.
  - Add `AuditJSONStrict` (`json:"audit_json_strict"`) and `AuditJSONInclusive` (`json:"audit_json_inclusive"`) for the two audit JSON artifacts. XLSX fields are added by task-024.
  - Add one shared URL builder used by both `jobLinks` and `get_artifact`, for example:
    - `artifactPath(mcpPath, jobID, kind) string` returning `<mcp-path>/artifacts/<jobID>/<kind>`.
    - `absoluteArtifactURL(base, mcpPath, jobID, kind) string` returning `<base><artifactPath>`.
  - Change `jobLinks` to accept the resolved base and MCP path instead of reading `MCPConfig` directly: `jobLinks(base, mcpPath, id string) startCrawlLinks`.
  - Document that links are always absolute and job-scoped.

### Artifact Resolution and Tool Output

- In `mcp_artifacts.go`:
  - Replace the `/api/crawl/...` URL construction in `artifactURL` with the shared absolute URL builder.
  - Thread the request context through `buildArtifact` and `resolveArtifact` so every descriptor carries an absolute URL: `buildArtifact(ctx, job, kind, inline)` and `resolveArtifact(ctx, jobID, kind, snapshot)`.
  - Apply the new builder to all five existing kinds: `csv`, `audit_strict_zip`, `audit_inclusive_zip`, `audit_json_strict`, `audit_json_inclusive`.
  - Preserve current inline behavior, filenames, sizes, content types, expiry, and error codes.
- In `mcp_tools.go`:
  - Change `startCrawlOutput` to accept the request context (or the already-resolved links) and pass it to `jobLinks`.
  - Update both `start_crawl` return paths, including the idempotent replay path, so links are identical for the same job.
  - Pass the request context into `buildArtifact` from `getArtifact`.
- Do not add the HTTP download route in this task; task-025 mounts it. This slice makes the URL contract correct and testable on its own.

### Security and Compatibility

- Never accept a base URL, host, or path from tool input.
- Do not expose filesystem paths in any link.
- Do not change existing UI `/api/crawl/...` download routes, CSV columns, audit artifacts, or inline CSV/JSON behavior.
- Keep `startCrawlLinks` JSON output additive except for removing `self`; existing clients reading `csv` and the two ZIP fields keep working.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go` and `server_test.go`
- Add tests:
  - `TestArtifactURLContract` in `mcp_test.go`:
    - Start a job with `cfg.ArtifactBaseURL = "https://crawler.example.com"` and assert every `start_crawl.links` value for `csv`, `audit_strict_zip`, `audit_inclusive_zip`, `audit_json_strict`, and `audit_json_inclusive` starts with `https://crawler.example.com/mcp/artifacts/<job_id>/` and ends with the correct kind.
    - Assert the output has no `self` key and contains no `gofrog://`.
    - Call `get_artifact` for each existing kind and assert its `url` is absolute and matches the corresponding `start_crawl` link where one exists.
    - Repeat with `cfg.ArtifactBaseURL = ""` and a test helper that exposes the `httptest` server URL; assert links use `http://127.0.0.1:<port>/mcp/artifacts/<job_id>/...`.
    - Repeat with `cfg.Path = "/crawl-api"` and assert `/crawl-api/artifacts/` appears in the links.
    - Assert an idempotent `start_crawl` replay returns the same absolute links for the same job ID.
  - `TestArtifactBaseFromRequest` in `server_test.go`:
    - Request-derived scheme/host from `X-Forwarded-Proto` and `X-Forwarded-Host`.
    - `r.TLS != nil` selects `https` when no forwarded proto is present.
    - Plain `r.Host` fallback selects the request host.
    - A configured `ArtifactBaseURL` wins over forwarded headers.
- **Test execution command:**
  ```bash
  go test -run 'TestArtifactURLContract|TestArtifactBaseFromRequest' .
  ```
- **Expected result:** The tests MUST fail. `startCrawlLinks` still has `self: "gofrog://jobs/<id>/status"`, URLs are relative or use `/api/crawl/...`, no request-derived resolver exists, and `get_artifact` cannot produce the new MCP artifact paths. Confirm the failure is caused by these missing behaviors, not by a test typo or unrelated compile error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `server.go` — add the request-scoped base middleware and wrap the mounted MCP handler.
  - `mcp.go` — add the context key/accessor and resolve the base from configuration or request context.
  - `mcp_types.go` — remove `Self`, add the two audit JSON link fields, and add the shared absolute URL builder.
  - `mcp_artifacts.go` — thread context through artifact resolution and use the shared URL builder for all five kinds.
  - `mcp_tools.go` — pass the request context into `startCrawlOutput` and `getArtifact`.
- **Test re-run command:**
  ```bash
  go test -run 'TestArtifactURLContract|TestArtifactBaseFromRequest' .
  ```
- **Expected result:** Tests MUST pass. Configured bases, request-derived bases, custom MCP paths, and replay paths all produce absolute job-scoped URLs; no `self`/`gofrog://` value is present; inline CSV and audit JSON still work.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Ensure there is exactly one URL builder; `jobLinks` and `get_artifact` must not duplicate string concatenation.
  - Normalize trailing slashes and empty paths in one helper.
  - Remove any remaining references to `gofrog://` or `/api/crawl/` in MCP URL generation.
  - Verify comments describe the absolute-URL contract.
- **Refactor validation command:**
  ```bash
  go test -run 'TestArtifactURLContract|TestArtifactBaseFromRequest' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions in UI routes or existing MCP tools.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing URL behavior (not a syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] `start_crawl` and `get_artifact` return absolute URLs for all five existing artifact kinds
- [ ] `GOFROG_MCP_ARTIFACT_BASE_URL` wins when set; request-derived scheme/host is used when unset
- [ ] Custom MCP paths are respected and normalized
- [ ] `links.self` is removed and no `gofrog://` URI is emitted
- [ ] Inline CSV and audit JSON behavior, filenames, content types, and expiry are unchanged
- [ ] The exact MCP endpoint and all existing UI routes remain unchanged
