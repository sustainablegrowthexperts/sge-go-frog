# Change Summary Specification: MCP AI Result Access

**Date:** 2026-11-10
**Status:** Approved for implementation
**Precedence:** This spec records the decisions agreed during clarification. It supplements, and does not replace, `specs/2026-11-10-go-frog-mcp-integration.md`.

---

## 1. Objective & Overview

Make go-frog's MCP surface usable end-to-end by an AI assistant: start crawls and internal-linking audits, poll job completion, retrieve structured results and audit data, and download every report artifact through absolute, job-scoped URLs under the existing MCP endpoint. This closes the current gap where MCP can describe artifacts but an AI cannot reliably fetch them, and makes the supported result formats directly available without requiring the web UI.

---

## 2. Scope Boundaries

### In Scope

| Item | Detail |
|---|---|
| Artifact download route | Serve completed MCP job artifacts from a subtree of the existing MCP endpoint: `<mcp-path>/artifacts/{job_id}/{kind}`. No new listener or separate route family. |
| Absolute artifact URLs | `start_crawl` links and `get_artifact.url` return absolute URLs, never bare paths. |
| All result formats | CSV, strict/inclusive audit ZIP, strict/inclusive audit JSON, and strict/inclusive audit XLSX are individually addressable. |
| Standalone XLSX | Expose `audit_xlsx_strict` and `audit_xlsx_inclusive` as first-class artifact kinds generated from the existing audit workbook logic. |
| Download behavior | `GET` and `HEAD` are supported. Responses carry the correct content type, sanitized filename, `Content-Disposition`, and length. |
| Ownership boundary | Downloads are restricted to MCP-created jobs. UI-created jobs remain inaccessible through MCP URLs. |
| Job-state enforcement | Artifacts are downloadable only when the job is `done`; unknown jobs, non-MCP jobs, unknown kinds, and expired artifacts fail safely. |
| Completion polling | `get_job` remains the completion mechanism; terminal status, phase, progress, and error details are part of the AI-facing contract. |
| Agent guidance | The MCP server provides concise workflow instructions covering start, poll, read, and download, including when archiving or auditing is required. |
| Tool descriptions and schemas | Tool descriptions and input schemas reflect absolute URLs, all supported artifact kinds, and the retrieval workflow. |
| Dead link cleanup | Remove the unreachable `gofrog://jobs/{id}/status` self link from `start_crawl` output because MCP resources remain out of scope. |
| Regression safety | Existing web UI routes, CSV columns, audit artifacts, retention, and CLI behavior remain unchanged. |
| Tests and docs | Unit, integration, protocol, and regression tests; README and operational documentation updates. |

### Out of Scope

| Item | Reason |
|---|---|
| Admin/management tools | Delete, retry, re-run, re-audit, and other management utilities are not required for AI-assisted audits and searches. |
| `cancel_job` and cancellation | Explicitly excluded from this change. |
| MCP resources and resource templates | `gofrog://` URIs remain excluded; the dead self link is removed rather than implemented. |
| Progress notifications | Polling through `get_job` remains the completion mechanism. |
| Blocking or wait-until-done tool calls | Completion stays poll-based; no new wait tool is added. |
| Prompts, sampling, roots, elicitation, and tasks | Explicitly excluded. |
| stdio transport and legacy SSE | Streamable HTTP remains the only transport. |
| Authentication and authorization changes | Access control remains outside this change; no token handling is added to the application. |
| Per-caller isolation, multiple tokens, admin scopes | Single shared workspace remains the deployment model. |
| Horizontal scaling and shared job stores | Single replica remains the deployment model. |
| Private/internal-network crawling | Public HTTP(S) only. |
| Full-text search index | Archive-backed search remains sufficient. |
| Arbitrary filesystem access and re-audit by path | Artifacts remain job-scoped and path input remains rejected. |
| Browser-rendered crawling and JavaScript execution | Existing crawler limitation. |
| Metrics, Prometheus, and health endpoints | Not required for this change. |
| Changes to the existing web UI, CSV columns, audit artifacts, or CLI | Regression guarantee. |
| New report formats beyond CSV, XLSX, JSON, and ZIP | Existing report formats cover the AI use cases. |
| Changes to retention, persistence, or storage layout | Existing behavior is preserved. |
| Changes to partial-result or failure policy | Existing behavior is preserved. |

---

## 3. Technical Architecture & Decisions

| Decision | Rationale |
|---|---|
| Mount artifact downloads beneath the configured MCP path at `<mcp-path>/artifacts/{job_id}/{kind}`. | Keeps all AI-facing result retrieval under the existing MCP surface and avoids introducing a second externally routed path. |
| Register the download handler as a subtree of the MCP endpoint while keeping `<mcp-path>` itself unchanged. | Preserves the Streamable HTTP MCP endpoint contract and avoids interference with tool calls. |
| Return absolute artifact URLs everywhere. | An AI can curl the returned URL directly; relative paths are not actionable to an external agent. |
| Resolve the public base URL from `GOFROG_MCP_ARTIFACT_BASE_URL` when configured, otherwise derive it from the incoming MCP request context. | Provides stable canonical URLs in configured deployments and usable URLs locally without extra configuration. |
| Support artifact kinds `csv`, `audit_strict_zip`, `audit_inclusive_zip`, `audit_json_strict`, `audit_json_inclusive`, `audit_xlsx_strict`, and `audit_xlsx_inclusive`. | Matches the existing report artifacts and adds direct XLSX retrieval. |
| Generate standalone XLSX with the same workbook builder used by the audit ZIP. | Guarantees the standalone workbook and the bundled workbook contain the same sheets and data. |
| Enforce the MCP ownership boundary in the download handler. | Prevents an AI from downloading browser-created jobs or probing for job existence. |
| Require a terminal `done` job before serving artifacts. | Avoids returning partial or incomplete reports. |
| Use `GET` and `HEAD` only; reject other methods with `405`. | Matches file retrieval semantics and supports clients that probe metadata before downloading. |
| Set content type, content disposition, and sanitized filenames per artifact kind. | Allows an AI to save files with correct names and extensions. |
| Keep the existing inline CSV/JSON behavior for small artifacts. | Preserves the existing tool contract for clients that prefer inline content. |
| Remove `links.self` from `start_crawl` because it advertises an unreadable `gofrog://` resource. | MCP resources are out of scope, so advertising a resource URI is misleading to agents. |
| Keep `get_job` as the only completion mechanism. | Existing polling already exposes all terminal states and error details; no blocking wait is introduced. |
| Add server-level workflow instructions and update tool descriptions. | An AI should be able to discover the start → poll → read → download sequence without outside documentation. |
| Leave the existing web UI download routes and artifact generation untouched. | The change is additive for MCP and must not alter human-facing behavior. |
| No application-level token validation or URL signing. | Authentication remains outside this change; URLs never embed credentials. |
| URLs remain valid only while the job and its artifacts remain within the existing retention window. | Reuses current storage and expiry behavior. |

### Artifact URL contract

```text
<scheme>://<host><mcp-path>/artifacts/<job_id>/<kind>
```

Examples:

```text
http://localhost:4374/mcp/artifacts/<job_id>/csv
http://localhost:4374/mcp/artifacts/<job_id>/audit_strict_zip
http://localhost:4374/mcp/artifacts/<job_id>/audit_json_inclusive
http://localhost:4374/mcp/artifacts/<job_id>/audit_xlsx_strict
```

### Download response contract

| Condition | Response |
|---|---|
| Valid completed MCP job and artifact | `200` with file bytes, `Content-Type`, `Content-Disposition`, and `Content-Length` |
| `HEAD` request for a valid artifact | `200` with headers and no body |
| Unknown job, UI-owned job, unknown kind, or expired artifact | `404` |
| Job exists but is not `done` | `409` |
| Unsupported HTTP method | `405` |
| Artifact generation failure | `500` without filesystem paths or internal details |

---

## 4. Affected Files

| File | Action |
|---|---|
| `server.go` | Modify — register the artifact download subtree under the configured MCP path and capture request-scoped base URL context. |
| `mcp.go` | Modify — expose the download handler, add server workflow instructions, and keep the existing Streamable HTTP endpoint unchanged. |
| `mcp_artifacts.go` | Modify — build absolute artifact URLs, resolve all artifact kinds including XLSX, implement the download handler, and enforce ownership/status rules. |
| `mcp_tools.go` | Modify — update tool descriptions and artifact-kind lists for absolute URLs and the full result surface. |
| `mcp_types.go` | Modify — add XLSX artifact kinds, extend link/output fields, and remove the unreachable self resource link. |
| `internal/auditbundle/bundle.go` | Modify — expose the existing workbook generation path for standalone XLSX artifacts. |
| `mcp_test.go` | Modify — add unit, protocol, download, ownership, and regression tests. |
| `server_test.go` | Modify — cover route mounting, request-derived URL behavior, and preservation of existing UI downloads. |
| `README.md` | Modify — document the AI download workflow, absolute URLs, supported artifact kinds, and completion polling. |
| `DEPLOYMENT.md` | Modify — document artifact download behavior and retention without changing existing deployment mechanics. |
| `.env.example` | Modify — clarify that `GOFROG_MCP_ARTIFACT_BASE_URL` controls the canonical absolute URL base, with request-derived fallback when unset. |

---

## 5. Proposed Task Breakdown

### Phase 1 — Artifact model and URL generation

1. **Add absolute URL resolution** — implement a request-scoped base URL resolver that prefers `GOFROG_MCP_ARTIFACT_BASE_URL` and otherwise derives the base from the incoming MCP request.
2. **Move artifact URLs under MCP** — build all artifact URLs as `<base><mcp-path>/artifacts/{job_id}/{kind}`.
3. **Add standalone XLSX kinds** — extend the artifact allowlist with `audit_xlsx_strict` and `audit_xlsx_inclusive`, and expose a reusable workbook-generation helper from `internal/auditbundle`.
4. **Resolve and cache XLSX artifacts** — generate XLSX files under the job-owned results directory using the same workbook data as the audit ZIP, with per-job/profile caching.
5. **Update artifact metadata** — add filenames and content types for XLSX; preserve existing CSV, ZIP, and JSON names and inline behavior.
6. **Update link surfaces** — make `start_crawl` links absolute and artifact-specific, make `get_artifact.url` absolute, and remove the unreachable `gofrog://` self link.

### Phase 2 — Download endpoint

7. **Implement the download handler** — parse `<mcp-path>/artifacts/{job_id}/{kind}`, validate the kind against the allowlist, and reject path-like input.
8. **Enforce MCP ownership** — resolve jobs through the existing MCP ownership boundary so UI jobs and unknown jobs return the same `404`.
9. **Enforce job state** — return `409` for jobs that are not `done`; return `404` for missing or expired artifacts.
10. **Serve artifact responses** — set content type, content disposition, filename, and length; support `GET`, `HEAD`, and range requests through the standard file-serving path.
11. **Map failures safely** — return `405` for unsupported methods and `500` for generation failures without leaking filesystem paths, internal errors, or stored data.
12. **Mount the subtree route** — register the handler beneath the configured MCP path only when MCP is enabled; leave the exact MCP endpoint and all existing routes untouched.

### Phase 3 — Agent-facing guidance

13. **Add server instructions** — describe the complete assistant workflow: start a crawl or audit, poll `get_job` until a terminal state, read results/audits/search output, then fetch artifact URLs.
14. **Document prerequisites in instructions** — state that HTML/text search requires `archive_html=true` or `audit=true`, and that audit retrieval requires `audit=true`.
15. **Update tool descriptions** — reflect absolute URLs, all seven artifact kinds, download semantics, and the fact that completion is checked through `get_job`.
16. **Align schemas and outputs** — ensure input enums, output fields, and link placeholders expose the same artifact surface as the download handler.

### Phase 4 — Tests, documentation, and acceptance

17. **Add unit tests** — cover URL generation with configured and request-derived bases, all artifact kinds, filenames, content types, and removal of the dead self link.
18. **Add integration tests** — start a job with the fake runner, poll to completion, call `get_artifact`, download CSV/ZIP/JSON/XLSX over HTTP, and verify `HEAD` and range behavior.
19. **Add negative tests** — cover unknown jobs, UI-owned jobs, unknown kinds, running jobs, expired artifacts, unsupported methods, and path-like input.
20. **Add regression tests** — verify existing UI download routes, CSV columns, audit bundle contents, MCP tool listing, and structured outputs remain unchanged.
21. **Update documentation** — document the artifact URL shape, supported formats, download workflow, completion polling, and retention behavior in `README.md`, `DEPLOYMENT.md`, and `.env.example`.
22. **Run the acceptance suite** — `go test ./...`, `go test -race ./...`, and `go vet ./...`; confirm every in-scope item has automated coverage or a documented manual check.
