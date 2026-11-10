# Change Summary Specification: go-frog MCP Integration

**Date:** 2026-11-10
**Status:** Approved for implementation
**Design reference:** `MCP.md`
**Precedence:** This spec records the decisions agreed during clarification. Where it conflicts with `MCP.md`, this spec wins.

---

## 1. Objective & Overview

Add an embedded Model Context Protocol (MCP) server to go-frog so MCP-capable AI agents (Claude) can start spider and list crawls, run internal-linking audits, poll job status, retrieve paginated structured results, search crawled page content, and download CSV/audit artifacts without using the human web UI. The feature reuses the existing crawl runners, job manager, keyword counter, and audit engine; it runs in the same process behind the existing oauth2-proxy with a shared bearer token, and it preserves the existing UI, CSV columns, audit artifacts, CLI wizard, and standalone `go-frog-audit` behavior.

---

## 2. Scope Boundaries

### In Scope

| Item | Detail |
|---|---|
| Embedded MCP server | Streamable HTTP MCP endpoint at `/mcp` in the existing go-frog process, sharing the job manager and crawl/audit pipeline with the web UI. |
| Seven MCP tools | `start_crawl`, `get_job`, `list_jobs`, `get_results`, `search_pages`, `get_audit`, `get_artifact`. |
| Spider and list modes | Start jobs in either mode; spider follows same-site links; list visits supplied URLs once. |
| Crawl options | Keywords, concurrency, audit (strict and inclusive), `archive_html`, per-job audit filters, per-job limits, `client_request_id`. |
| Structured JSON contracts | Snake_case DTOs, RFC 3339 UTC timestamps, millisecond durations, pagination envelope, stable error codes. |
| Job lifecycle | Status, phase, timestamps, page progress, current URL, limits, truncation, audit state, artifact readiness. |
| Retention and persistence | Completed MCP jobs and artifacts survive restarts/deploys for 24 hours after completion; running jobs are never reaped; interrupted jobs are marked on startup. |
| Results | Filtering, sorting, pagination, keyword map, optional audit join, depth/discovery from the manifest, raw inlinks and image alts. |
| Content search | Metadata search without an archive; HTML/text search with an archive; literal and RE2 regex; snippets; server caps. |
| Audit access | Strict and inclusive profiles; summary, pages, links, blocks, anchors, PageRank, redirects, failed, sitemap, structure, and `all`. |
| Artifacts | CSV, strict ZIP, inclusive ZIP, and audit JSON; authenticated URLs; inline content under the size cap. |
| Server-enforced limits | Pages, depth, list URLs, duration, concurrency, global/per-token jobs, retained jobs, response size, search caps, pattern length, URL length, keyword count, request size, and rate limit. |
| Security | Public HTTP(S) only; SSRF blocking for private/loopback/link-local/metadata targets, including redirects and sitemaps; no arbitrary filesystem paths; no token/path leakage in errors or logs. |
| Auth boundary | Authentication is enforced by the deployment's oauth2-proxy with one shared bearer token; the app assumes authenticated requests and does not implement per-caller ownership. |
| UI/MCP separation | MCP lists only MCP-created jobs; UI jobs remain browser-only; no UI job-list changes. |
| Existing behavior unchanged | Existing HTML/HTMX flow, CSV columns, audit artifacts, `--cli`, and `go-frog-audit` behavior remain unchanged. |
| Graceful shutdown | SIGINT/SIGTERM handling; no half-written archives; running jobs marked interrupted. |
| Tests and docs | Unit, integration, protocol, and security tests; passing `go test ./...`, `go test -race ./...`, and `go vet ./...`; README, DEPLOYMENT, client config, and runbook updates. |

### Out of Scope

| Item | Reason |
|---|---|
| `cancel_job` and cancellation | Deferred to phase 2; page, depth, and duration caps are the v1 guardrail. |
| MCP resources (`gofrog://` URIs) | Optional layer excluded from the v1 Definition of Done. |
| Progress notifications | Optional layer; polling via `get_job` is the v1 baseline. |
| stdio transport and `--mcp` mode | Claude connects over HTTP; optional layer excluded. |
| OAuth 2.1 and dynamic client registration | Authentication is handled by oauth2-proxy; no app-level OAuth. |
| Per-caller isolation, multiple tokens, admin scopes | One shared token and one shared workspace by decision. |
| Multiple replicas, shared job store, database | Single replica; in-memory plus per-job filesystem persistence. |
| Private/internal/staging crawling | Public sites only; no private-network opt-in in v1. |
| Full-text index (Bleve, Elasticsearch) | Archive-scan search is sufficient for the 5,000-page default. |
| Arbitrary filesystem access and re-audit by path | Jobs address artifacts by job ID; path input is rejected. |
| Browser-rendered crawling and JavaScript execution | Existing limitation; not part of MCP. |
| MCP tasks, prompts, sampling, roots, elicitation | Not needed for the v1 polling design. |
| Legacy SSE transport | Streamable HTTP only. |
| `--mcp-bridge` | Claude connects directly. |
| Metrics/Prometheus and health endpoints | Beyond the v1 Definition of Done; structured logs only. |
| Changing existing UI, CSV columns, audit artifacts, or CLI behavior | Regression guarantee. |
| Public multi-tenant SaaS control plane | Single-tenant deployment. |

---

## 3. Technical Architecture & Decisions

| Decision | Rationale |
|---|---|
| Embed MCP in the existing go-frog process, sharing the job manager and crawl/audit pipeline. | Agent jobs are the same jobs the app already knows; one deployment; no sidecar scraping the HTML API. |
| Serve MCP over Streamable HTTP at `/mcp` on the existing listener behind oauth2-proxy. | Claude connects remotely; authentication is already enforced at the proxy; one port and one deployment. |
| Pin the official Go SDK `github.com/modelcontextprotocol/go-sdk`; isolate SDK types behind a small adapter. | The SDK handles JSON-RPC, schemas, sessions, and transport; the adapter limits protocol churn. |
| Async job model: `start_crawl` returns immediately; `get_job` polls; no blocking tool call. | Crawls and audits take minutes; MCP clients time out on long calls. |
| Keep JSON DTOs separate from `crawlJob`; use snake_case, RFC 3339 UTC, and millisecond durations. | The HTML view and the MCP view can evolve independently, and the contract stays stable. |
| Add job `phase`, `startedAt`, `finishedAt`, `owner`, `limits`, `truncated`, `limit_reason`, `archiveHTML`, and `clientRequestID`. | Agents need to distinguish crawl, sitemap, and audit phases and understand truncation. |
| New MCP job IDs use 128 bits; existing UI job IDs may retain their current shape. | IDs are identifiers, not authorization; larger IDs reduce guessability. |
| Persist completed MCP jobs under a per-job directory (`results/jobs/<id>/`) with `job.json`, page rows, artifacts, and audit directories; use atomic writes. | Satisfies 24-hour retention across restarts without adding a database. |
| Reaper: never delete running jobs; expire MCP jobs 24 hours after `finishedAt`; keep UI jobs at their current 30-minute retention after completion; evict the oldest finished MCP jobs when the retained-job cap is exceeded. | Fixes the current reaper bug that deletes running jobs while preserving existing UI behavior. |
| Startup recovery: mark previously running MCP jobs as `error` with code `interrupted`; keep completed jobs. | No resume semantics; the state is honest after a restart or deploy. |
| One shared token and one shared workspace; no app-level owner filtering. Jobs created by the UI use owner `ui`; MCP tools only read MCP-created jobs. | Matches the deployment decision and keeps human and agent jobs separate. |
| Limits: 5,000 pages, depth 10, 1,000 list URLs, 30-minute duration, concurrency 20, 4 global jobs, 2 per token (2 concurrent with one shared token), 100 retained jobs, 20 keywords of 100 characters, 512-character patterns, 500 result/audit limits, 1 MB inline artifacts, 1 MB requests, 2,048-character URLs, 5 MB responses, and 60 tool calls per minute. All are configurable. | Server-enforced safety; per-job limits cannot raise server maxima. Caps apply to MCP jobs; UI behavior is unchanged. |
| SSRF policy: allow only HTTP(S); resolve the host; reject loopback, private, link-local, unspecified, multicast, IPv4-mapped IPv6, and cloud-metadata targets; validate redirects, sitemaps, and list URLs; use a custom `DialContext`. | Prevents the hosted endpoint from being used to scan internal networks. Public-only; no private-network opt-in in v1. |
| Content search: archive scan; scopes `metadata`, `html`, `text`, and `all`; literal search is case-insensitive by default; regex uses Go RE2; snippets and caps; worker pool; no inverted index. | Meets the search use case without adding Bleve/Elasticsearch; 5,000 pages is manageable. |
| Audit access: read `audit.json` from the job-owned directory; support strict and inclusive; deterministic sorting; section pagination; cache by path and mtime; `links` remains content-only; full inventory is available through the ZIP. | Reuses the deterministic audit engine and avoids returning large JSON blobs. |
| Artifacts: CSV generated at request time; audit ZIPs via `auditbundle.WriteFile`; audit JSON metadata; authenticated URL behind the proxy; 1 MB inline cap; no arbitrary paths. | Reuses existing export and bundle code; protects access without new signing infrastructure. |
| Error contract: `isError: true` plus `{code, message, retryable, details}`. Codes are `invalid_argument`, `unauthorized`, `forbidden`, `job_not_found`, `job_not_finished`, `audit_not_available`, `search_not_available`, `limit_exceeded`, `conflict`, and `internal_error`. | Stable agent behavior; no stack traces, filesystem paths, tokens, or proxy credentials. |
| Idempotency: map `owner + client_request_id` to a job for the retention window; identical input returns the existing job; different input returns `conflict`. | Agents retry after timeouts and must not create duplicate crawls. |
| Concurrency: per-token semaphore plus a global semaphore; queue up to twice the global limit, then reject with `retry_after_ms`. | Bounded resource use; agents can retry when the server is busy. |
| Graceful shutdown: trap SIGINT/SIGTERM; stop accepting new jobs; mark running jobs interrupted; close archive writers; shut down the HTTP server with a timeout. | Avoids half-written archives and orphaned temporary files. |
| Structured logging with `log/slog`: log token identity if the proxy provides one, job ID, tool, target host, outcome, pages/bytes, and error code; never log the token, page bodies, or proxy credentials. | Operability without leaking secrets or personal data. |
| Testability: wrap `runSpider` and `runList` behind a `crawlRunner` interface; use a fake runner, in-memory job manager, SDK client over `httptest`, a fake clock, golden schemas, temporary directories, and race tests. | Deterministic tests without network dependence. |
| Configuration: flags plus environment variables for MCP enable/path, limits, TTL, results directory, archive default, and public artifact base URL; no config file in v1. | Matches the current manual flag style and keeps the surface small. |
| Authentication: rely on oauth2-proxy; do not implement token validation, scopes, or per-caller ownership in the app; never log the `Authorization` header. | Matches the deployment decision and avoids duplicating proxy auth. |
| No database, no message queue, and no horizontal scaling. | Single replica and v1 scope. |
| Per-job audit filters reuse the existing `audit.Options.Filters` (`nav_substrings`, `exclude_substrings`, `explicit`); no new `force_content` or `force_exclude` schema in v1. | Matches the `start_crawl` tool schema and the existing auditor API. |

---

## 4. Affected Files

| File | Action |
|---|---|
| `specs/2026-11-10-go-frog-mcp-integration.md` | Create — this specification. |
| `go.mod` | Modify — add and pin the official MCP Go SDK dependency. |
| `go.sum` | Modify — record the new dependency checksums. |
| `main.go` | Modify — add MCP flags/environment parsing, `ArchiveHTML` setting, MCP startup, and graceful shutdown. |
| `server.go` | Modify — extract a shared app/job manager, mount `/mcp`, add manager methods (`list`, `runningCount`, `createWithOwner`, `delete`), add job phases/timestamps/owner/limits, fix the reaper, and add persistence hooks. |
| `spider.go` | Modify — add context and limits, archive when `archive_html` is set even without audit, add phase/progress callbacks, use the SSRF-validated collector, and enforce page/depth/duration caps. |
| `list.go` | Modify — add context and limits, archive when `archive_html` is set, add phase/progress callbacks, and enforce the list-URL cap. |
| `scrape.go` | Modify — record archive entries whenever an archive is enabled, enforce page/depth limits, and add phase/progress callbacks. |
| `sitemap.go` | Modify — use the SSRF-validated client/dialer for robots.txt and sitemap fetches. |
| `mcp.go` | Create — MCP server construction, Streamable HTTP transport, and lifecycle. |
| `mcp_tools.go` | Create — registration and handlers for the seven tools. |
| `mcp_types.go` | Create — input/output DTOs, pagination, job status, phase constants, and error envelopes. |
| `mcp_limits.go` | Create — quotas, semaphores, rate limiter, and limit validation. |
| `mcp_search.go` | Create — archive-backed metadata/HTML/text search, snippets, and caps. |
| `mcp_audit.go` | Create — `audit.json` loading, sectioning, sorting, pagination, and caching. |
| `mcp_artifacts.go` | Create — CSV/ZIP/audit JSON metadata, authenticated URLs, and inline caps. |
| `mcp_ssrf.go` | Create — public-target validation, custom dialer, and redirect/sitemap checks. |
| `job_store.go` | Create — per-job directory persistence, atomic writes, startup recovery, and cleanup. |
| `job_runner.go` | Create — `crawlRunner` interface around `runSpider`/`runList` for tests. |
| `mcp_test.go` | Create — unit, protocol, and integration tests for the MCP layer. |
| `job_store_test.go` | Create — persistence, recovery, TTL, and cleanup tests. |
| `mcp_ssrf_test.go` | Create — private-IP, redirect, sitemap, and DNS-rebinding tests. |
| `server_test.go` | Modify — add job lifecycle, persistence, and reaper tests while preserving existing fragment tests. |
| `audit_integration_test.go` | Modify — cover audit access and search through the MCP layer. |
| `sitemap_integration_test.go` | Modify — cover sitemap discovery with the SSRF-validated client. |
| `.env.example` | Create — template for MCP enable/path, limits, TTL, results directory, and artifact base URL; no real secrets; authentication remains at oauth2-proxy. Developer copies it to `.env` manually. |
| `compose.yml` | Modify — add the `/results` volume and MCP environment variables; keep the proxy-facing port/path configuration. |
| `README.md` | Modify — document MCP setup, the seven tools, limits, retention, and client configuration. |
| `DEPLOYMENT.md` | Modify — document the results volume, proxy routing, MCP endpoint, restart behavior, and retention. |

---

## 5. Proposed Task Breakdown

### Phase 0 — Protocol spike and scaffolding

1. **Pin the MCP SDK** — add the official Go SDK, verify `initialize` and `tools/list` over an in-memory or `httptest` transport, and isolate SDK-specific types behind an adapter.
2. **Define the MCP contracts** — create `mcp_types.go` with job-status, page-row, audit-section, search-match, pagination, and error envelopes, plus phase/status constants.
3. **Build the MCP server shell** — create `mcp.go` with server construction, server metadata, `/mcp` Streamable HTTP handling, and a lifecycle hook for shutdown.
4. **Introduce the runner interface** — create `job_runner.go` and adapt `runSpider`/`runList` behind a `crawlRunner`; add a controllable fake runner for tests.
5. **Share the application core** — extract a shared app/job-manager constructor from `startServer`, mount `/mcp` beside the existing routes, and verify the web UI still behaves exactly as before.
6. **Add configuration and deployment scaffolding** — add flags/environment variables for MCP enable/path, limits, TTL, results directory, and artifact base URL; create `.env.example`; update `compose.yml` with the `/results` volume and MCP env vars.

### Phase 1 — Job lifecycle and read tools

7. **Extend the job model** — add phase, start/finish timestamps, owner, limits, truncation fields, archive flag, and client request ID; keep `jobView` and the HTML fragments unchanged.
8. **Add manager operations** — implement `createWithOwner`, `list`, `runningCount`, `delete`, and phase updates; filter MCP lists to MCP-created jobs.
9. **Fix retention** — never reap running jobs; expire MCP jobs 24 hours after completion; keep UI jobs at their current retention; evict the oldest finished MCP jobs at the retained-job cap.
10. **Add persistence** — create `job_store.go` with per-job directories, atomic `job.json` and page-row writes, startup recovery that marks running jobs interrupted, and expiry cleanup.
11. **Enforce limits and truncation** — apply page, depth, duration, URL, concurrency, and response caps; stop scheduling at a cap; set `truncated` and `limit_reason`; preserve partial results.
12. **Implement `start_crawl`** — validate mode, URLs, keywords, concurrency, audit, `archive_html`, audit filters, per-job limits, and `client_request_id`; enforce server maxima; create and launch the job; return the job ID, phase, poll hint, and artifact links.
13. **Implement `get_job`** — return status, phase, timestamps, pages crawled, current URL, keywords, truncation, limits, audit state, artifact readiness, and error.
14. **Implement `list_jobs`** — filter by status and mode, paginate, sort newest first, and expose only MCP-created jobs.
15. **Implement `get_results`** — require completion; filter by status code, URL, title, keyword, and minimum hits; sort; paginate; build the keyword map; join manifest depth/discovery; optionally join audit metrics by normalized URL; cap response size.
16. **Add idempotency** — store `owner + client_request_id` to job mappings for the retention window; return the existing job for identical input and `conflict` for different input.
17. **Add concurrency control** — implement the per-token and global semaphores, a bounded wait queue, and `limit_exceeded` with `retry_after_ms`.

### Phase 2 — Crawl, audit, search, and artifacts

18. **Add `archive_html`** — extend `WizardSettings` and both runners so an archive can be created without audit in spider and list mode; ensure the manifest is closed and available for depth/discovery.
19. **Add SSRF protection** — implement public HTTP(S) validation, private/loopback/link-local/metadata blocking, a custom dialer, and validation of redirects, sitemap URLs, and list URLs.
20. **Wire limits into the crawlers** — pass context and limits through `runSpider`, `runList`, and page recording; enforce page/depth/duration caps; emit phase and progress callbacks; keep CSV and page-row behavior unchanged.
21. **Implement `get_audit`** — load strict and inclusive `audit.json`; expose summary, pages, links, blocks, anchors, PageRank, redirects, failed, sitemap, structure, and `all`; apply section filters, deterministic sorting, pagination, `page_key`/`url`/`raw_url`/`final_url`, and the content-link caveat.
22. **Implement `search_pages`** — search metadata without an archive; search raw HTML or visible text with an archive; support literal and RE2 regex, case sensitivity, snippets, context length, per-page match caps, and truncation; return `search_not_available` with a clear hint when no archive exists.
23. **Implement `get_artifact`** — expose CSV, strict ZIP, inclusive ZIP, and audit JSON; require completion; build CSV at request time; build ZIPs with `auditbundle.WriteFile`; return an authenticated URL plus inline content below the cap; reject arbitrary paths.
24. **Harden the tool handlers** — enforce the MCP-job ownership boundary, map domain errors to the stable error contract, cap request/response sizes, and ensure no paths or tokens leak.
25. **Add structured logging** — log tool calls, job lifecycle events, phases, artifacts, auth failures where visible, and limit events with the agreed fields; never log tokens or page bodies.

### Phase 3 — Hardening, tests, and acceptance

26. **Implement graceful shutdown** — trap SIGINT/SIGTERM, stop accepting work, mark running jobs interrupted, close archives, and shut down the HTTP server with a timeout.
27. **Add protocol tests** — initialize, tools/list, tools/call, structured content, text summaries, domain errors, and golden JSON schemas.
28. **Add job and persistence tests** — create/list/get/delete, TTL, retained cap, fake-clock reaping, restart recovery, interrupted jobs, and atomic writes.
29. **Add security tests** — private/loopback/link-local/metadata targets, redirect and sitemap validation, DNS rebinding, path traversal, regex/pattern caps, request/response caps, and rate limits.
30. **Add integration tests** — run against an `httptest` site: spider start, polling, results, strict/inclusive audit, content search, artifacts, list mode, `archive_html`, and audit filters.
31. **Add regression tests** — existing HTML fragments, CSV columns, audit artifact contents, `--cli`, and `go-frog-audit` behavior remain unchanged.
32. **Update documentation** — README, DEPLOYMENT, client configuration examples, limits/retention, proxy routing, and the restart/runbook procedure.
33. **Run the acceptance suite** — `go test ./...`, `go test -race ./...`, and `go vet ./...`; verify every item in `MCP.md` §23; document any deliberate deviation in this spec.
