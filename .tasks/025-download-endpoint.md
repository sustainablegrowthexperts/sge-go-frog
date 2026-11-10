---
id: task-025
title: "[Slice] AI downloads every artifact over HTTP from the MCP artifact subtree"
status: completed
depends_on: ["task-024"]
target_files:
  - "server.go"
  - "mcp.go"
  - "mcp_artifacts.go"
  - "mcp_test.go"
  - "server_test.go"
---

## Objective

**Objective:** A client can `GET` or `HEAD` any completed MCP job's CSV, strict/inclusive audit ZIP, strict/inclusive audit JSON, or strict/inclusive audit XLSX at `<mcp-path>/artifacts/<job_id>/<kind>` and receives the correct bytes and headers, while unknown jobs, UI-owned jobs, unknown kinds, unfinished jobs, expired artifacts, unsupported methods, and generation failures map to the documented `404`/`409`/`405`/`500` responses without leaking filesystem paths.

## Layer-by-Layer Changes

### Route Mounting

- In `server.go`, when `cfg.MCP.Enabled` is true:
  - Keep the exact MCP endpoint registration unchanged: `mux.Handle(cfg.MCP.Path, ms.Handler())`.
  - Additionally register a subtree for downloads: `mux.Handle(artifactSubtreePattern(cfg.MCP.Path), ms.ArtifactHandler())`.
  - Normalize the MCP path before constructing the pattern so `/mcp` and `/mcp/` both mount at `/mcp/artifacts/`.
  - Do not register the route when MCP is disabled, and do not add a catch-all that could shadow the web UI.
- In `mcp.go`, expose `ArtifactHandler() http.Handler` backed by a handler in `mcp_artifacts.go`. The exact Streamable HTTP handler returned by `Handler()` must remain unchanged.

### Request Parsing and Validation

- In the download handler:
  - Allow only `GET` and `HEAD`. Any other method returns `405` with `Allow: GET, HEAD` and no body.
  - Strip the `<mcp-path>/artifacts/` prefix and require exactly two non-empty path segments: job ID and kind.
  - Reject empty segments, extra segments, trailing slashes, encoded path separators, `.`/`..`, backslashes, NUL bytes, and any value accepted by `looksLikeArtifactPath`.
  - Reject unknown kinds against `getArtifactKinds`.
  - Every malformed or unknown value returns the same generic `404`; never echo the caller's path or a filesystem location.

### Ownership and Job State

- Resolve the job with the existing MCP ownership boundary:
  - Use `s.jobs.get(jobID)` plus `job.ownedBy(ownerMCP)`, or an equivalent helper that produces the same result as `lookupMCPJob`.
  - Unknown jobs and UI-created jobs MUST both return an identical `404`; do not reveal that a browser job exists.
- Take one artifact snapshot and enforce state:
  - If the job exists but its status is not `done`, return `409`.
  - If the artifact file is missing or the job/artifact has expired, return `404`.
  - Unknown kind on a valid completed job returns `404`.
- Do not require or add application-level authentication. The route stays behind the existing oauth2-proxy and uses no URL signing or credentials.

### Artifact Response

- Reuse the resolution/generation path from tasks 023 and 024 so the handler serves exactly what `get_artifact` describes:
  - CSV is regenerated on demand and written atomically under the job-owned directory.
  - Audit ZIPs and XLSX workbooks are built on demand and cached by source-directory mtime.
  - Audit JSON is read from the job-owned report directory.
- Open the resolved file, `stat` it, and `defer` close.
- Set response headers:
  - `Content-Type` from the artifact descriptor.
  - `Content-Disposition: attachment; filename="<sanitized>"` using `filepath.Base` plus the existing slug/filename sanitization. Reject or strip CR, LF, and quotes.
  - For a full response, `Content-Length` must match the file size; `http.ServeContent` supplies the partial length for range responses.
- Serve with `http.ServeContent(w, r, filename, modTime, file)` so:
  - `GET` returns `200` with bytes.
  - `HEAD` returns `200` with headers and no body.
  - Range requests return `206` with `Content-Range`, and invalid ranges return `416`.
  - `Accept-Ranges` and `Last-Modified` are set by the standard library.
- Add `X-Content-Type-Options: nosniff`; do not add permissive CORS headers.

### Failure Mapping

- Map failures without leaking internals:
  - Malformed path, unknown job, UI-owned job, unknown kind, missing file, expired job/artifact: `404`.
  - Job exists but is not `done`: `409`.
  - Method not `GET`/`HEAD`: `405` plus `Allow`.
  - CSV writer failure, workbook/ZIP builder failure, or file-open/read failure after a valid resolution: `500` with a short generic body such as `artifact could not be generated`.
- Distinguish "audit directory/artifact missing" (`404`) from "builder failed" (`500`) in the resolver. A missing audit directory is unavailable; a present directory whose CSV/JSON input is broken is a generation failure.
- Implement that distinction with internal sentinel or typed errors (for example `errArtifactUnavailable` and `errArtifactGeneration`), then translate them back to the existing `audit_not_available` and `internal_error` tool codes in `buildArtifact`. The `get_artifact` tool contract must not change for tool clients.
- Log the job ID, kind, status, and outcome server-side. Never log or return the resolved path, the target audit directory, or the underlying error text.
- Keep the existing `get_artifact` tool error codes unchanged for tool clients; the HTTP status mapping is specific to the download handler.

### Compatibility

- Leave all existing web UI routes untouched: `/api/crawl/<id>/download`, `/api/crawl/<id>/audit/bundle.zip`, and `/api/crawl/<id>/audit/inclusive/bundle.zip` must return the same bytes and headers as before.
- Leave the exact MCP endpoint and tool behavior unchanged.
- Leave retention, storage layout, and artifact generation behavior unchanged.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test files to create/update:** `mcp_test.go` and `server_test.go`
- Add tests:
  - `TestArtifactDownloadEndpoint` in `mcp_test.go`:
    - Add a test helper that returns the app and the `httptest` server base URL without changing the signature of `newMCPTestSession`.
    - Start a crawl with the immediate fake runner, poll `get_job` until `done`, take the `csv` link from `start_crawl`, and `GET` it. Assert `200`, `text/csv`, an attachment filename ending in `.csv`, a non-zero `Content-Length`, and non-empty body.
    - Create a done audit job with strict and inclusive fixtures (`writeAuditBundleFixture`, `createDoneResultsJob`) and `GET` all six audit kinds plus `csv`. Assert the expected status, content type, filename extension, and non-zero body.
    - `HEAD` the CSV and an audit artifact: assert `200`, the same `Content-Type`/`Content-Disposition`/`Content-Length`, and an empty body.
    - Range request `Range: bytes=0-9`: assert `206`, a `Content-Range` header, and exactly 10 bytes.
  - `TestArtifactDownloadErrors` in `mcp_test.go`:
    - Unknown job ID → `404`.
    - UI-owned completed job → `404`, with a body indistinguishable from an unknown job.
    - Unknown kind → `404`.
    - Path-like job ID, path-like kind, extra path segment, and trailing slash → `404`.
    - Running job (paused fake runner) → `409`.
    - Delete the resolved artifact file after completion → `404`.
    - Corrupt the audit directory so the ZIP/XLSX builder fails (for example remove `audit-pages.csv` after readiness) → `500` with a generic body that contains no filesystem path or internal error text.
    - `POST`, `PUT`, and `DELETE` → `405` with `Allow: GET, HEAD`.
  - `TestArtifactRouteMounting` in `server_test.go`:
    - MCP disabled → `/mcp/artifacts/<id>/csv` returns `404` and no handler is mounted.
    - Custom MCP path `/crawl-api` → downloads work at `/crawl-api/artifacts/...`.
    - The exact MCP endpoint still completes `initialize`/`tools/list`, and the artifact subtree does not interfere with it.
    - Existing UI CSV and audit ZIP downloads still return their previous content type, filename, and bytes.
- **Test execution command:**
  ```bash
  go test -run 'TestArtifactDownload|TestArtifactRouteMounting' .
  ```
- **Expected result:** The tests MUST fail because `<mcp-path>/artifacts/{job_id}/{kind}` is not mounted, no HTTP artifact handler exists, and requests currently return `404`. Confirm the failure is due to the missing route/handler, not a test setup error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `server.go` — mount the artifact subtree only when MCP is enabled; normalize the MCP path.
  - `mcp.go` — expose `ArtifactHandler()`.
  - `mcp_artifacts.go` — parse the path, enforce ownership/state/kind, resolve or generate the artifact, map failures to HTTP statuses, set headers, and serve with `http.ServeContent`.
  - `mcp_test.go` — download, header, HEAD, range, and negative tests.
  - `server_test.go` — route mounting, MCP-disabled, custom-path, and UI regression tests.
- **Test re-run command:**
  ```bash
  go test -run 'TestArtifactDownload|TestArtifactRouteMounting' .
  ```
- **Expected result:** Tests MUST pass. Every kind downloads with the right headers, `HEAD` and ranges work, the ownership/status/method/error contract holds, and existing UI routes are unchanged.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract path splitting, status mapping, and safe header writing into small helpers so the handler is readable.
  - Ensure the download resolver and `get_artifact` share one kind-to-descriptor path and cannot drift.
  - Remove duplicated header/error strings; centralize generic error bodies.
  - Verify no debug logging of paths, internal errors, or artifact contents.
- **Refactor validation command:**
  ```bash
  go test -run 'TestArtifactDownload|TestArtifactRouteMounting' . && go test ./...
  ```
- **Expected result:** All previously passing tests still pass. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to the missing route/handler
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] `GET` and `HEAD` work for CSV, both audit ZIPs, both audit JSONs, and both audit XLSX files
- [ ] `Content-Type`, sanitized `Content-Disposition`, and `Content-Length` are correct
- [ ] Range requests return `206` and `HEAD` returns no body
- [ ] Unknown jobs and UI-owned jobs both return an indistinguishable `404`
- [ ] Non-`done` jobs return `409`; unsupported methods return `405` with `Allow`
- [ ] Missing/expired artifacts return `404`; generation failures return `500` without paths or internal details
- [ ] The subtree is mounted only when MCP is enabled and a custom MCP path works
- [ ] The exact MCP endpoint and existing UI download routes remain unchanged
