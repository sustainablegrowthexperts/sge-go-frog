# go-frog MCP Integration — Design & Feasibility

**Status:** design proposal only. No production code has been changed. This document is the only addition.

**Question answered:** how hard would it be to let AI agents (MCP clients) send crawl, internal-linking-audit, and string-search jobs to go-frog from their own windows?

**Assumption:** the agents are MCP-capable hosts (Claude Desktop, Cursor, a custom agent, etc.) and go-frog acts as the MCP server. This is not browser automation of the existing UI; the agent calls tools, not HTML forms.

**Short answer:** the protocol layer is easy; the job lifecycle, result contracts, search semantics, and security are the real work. A solid single-user v1 is roughly **3–5 days**. A multi-user, internet-exposed deployment with persistence, quotas, and ownership is roughly **1–2 weeks**.

---

## Contents

1. [Executive summary](#1-executive-summary)
2. [Goals and non-goals](#2-goals-and-non-goals)
3. [What exists today](#3-what-exists-today)
4. [Gap analysis](#4-gap-analysis)
5. [MCP background](#5-mcp-background)
6. [Architecture options](#6-architecture-options)
7. [Recommended architecture](#7-recommended-architecture)
8. [Tool catalog](#8-tool-catalog)
9. [Resource catalog](#9-resource-catalog)
10. [Data contracts](#10-data-contracts)
11. [Job lifecycle changes](#11-job-lifecycle-changes)
12. [Search design](#12-search-design)
13. [Audit design](#13-audit-design)
14. [Security](#14-security)
15. [Configuration](#15-configuration)
16. [Client setup](#16-client-setup)
17. [Observability and operations](#17-observability-and-operations)
18. [Testing plan](#18-testing-plan)
19. [Rollout plan](#19-rollout-plan)
20. [Effort estimate](#20-effort-estimate)
21. [Risks and mitigations](#21-risks-and-mitigations)
22. [Open decisions](#22-open-decisions)
23. [Definition of done](#23-definition-of-done)
24. [Appendices](#24-appendices)

---

## 1. Executive summary

### 1.1 Verdict

The codebase is unusually well shaped for this. The app already has:

- a single Go binary with an embedded web UI;
- an in-memory async job manager with status, progress, errors, and results;
- reusable crawl runners (`runSpider`, `runList`) that know nothing about HTTP;
- keyword counting over raw HTML;
- a deterministic internal-linking auditor that already emits JSON-tagged structs and CSV/Excel bundles.

An MCP server is mostly a **thin tool layer over those existing capabilities**, plus a JSON-facing result contract. There is no database, no message queue, and no frontend framework to work around. The official Go SDK handles JSON-RPC, schemas, sessions, and both stdio and Streamable HTTP transports.

### 1.2 What makes it easy

| Area | Why it is easy |
|---|---|
| Crawling | `runSpider()` / `runList()` already accept settings and a progress callback |
| Async jobs | `jobManager` already creates jobs, tracks page counts, and stores results |
| Internal linking audit | `auditpkg.Analyze()` is deterministic, offline, and returns structured data |
| Deployment | One binary; MCP can live in the same process as the web UI |
| Protocol | Official Go SDK or `mark3labs/mcp-go`; no hand-written JSON-RPC needed |
| Existing UI | MCP can be added alongside it without changing the HTML/HTMX flow |

### 1.3 What makes it non-trivial

| Area | Why it needs work |
|---|---|
| Machine-readable output | Current job/status responses are HTML fragments; MCP needs JSON DTOs and pagination |
| Job retention | Jobs are in memory and reaped 30 minutes after creation, even while running |
| Search | Current keywords are counted at crawl time; post-hoc “find this string” needs archived HTML |
| Audit access | Only summaries and directories are retained; full `audit.json` must be read or cached |
| Limits | No max pages, depth, duration, URL count, or concurrent jobs |
| Auth | The app has no auth; a common deployment relies on oauth2-proxy for the human UI |
| Safety | Crawling arbitrary URLs from a server is an SSRF primitive; agents make it automatable |
| Multi-user | Jobs are global; ownership and artifact access need a model |

### 1.4 Recommended shape

Embed a **Streamable HTTP MCP endpoint** in the existing go-frog process, sharing the same `jobManager` as the web UI. Optionally add a `--mcp` stdio mode for local agents. Do **not** build a separate sidecar over the current HTML API.

### 1.5 Effort at a glance

| Scope | Estimate |
|---|---|
| Protocol spike (one tool, stdio) | 0.5–1 day |
| Core tools: start, status, results, list | 1–1.5 days |
| HTTP transport, token auth, limits, TTL | 1–1.5 days |
| Audit tools, archive search, artifacts | 1–1.5 days |
| Hardening, tests, docs | 1–2 days |
| **Solid single-user v1** | **~3–5 days** |
| **Multi-user production** | **~1–2 weeks** |

Assumes one experienced Go developer familiar with the codebase, no major design churn, and client compatibility tested near the end.

---

## 2. Goals and non-goals

### 2.1 Goals

1. Let an MCP-capable agent start a spider crawl, list crawl, or audit job.
2. Let the agent poll status and progress without holding a long-lived tool call open.
3. Let the agent retrieve paginated, structured results and filter them.
4. Let the agent read audit summaries and report sections.
5. Let the agent search crawled page content for strings, with snippets.
6. Let the agent download the existing CSV and audit ZIP artifacts.
7. Keep the human web UI behavior unchanged.
8. Keep the deployment as one process and one image.
9. Provide enough auth, limits, and isolation to expose the endpoint safely.
10. Make failures understandable to an agent: stable error codes, actionable messages.

### 2.2 Non-goals for v1

- A public multi-tenant SaaS control plane.
- A distributed job queue or multiple worker replicas.
- Browser-rendered crawling / JavaScript execution.
- Arbitrary filesystem access (including “audit this path”).
- Full-text indexing across every historical crawl.
- OAuth 2.1 / dynamic client registration (static bearer token first).
- Cancellation of an in-flight Colly crawl (best-effort later).
- Replacing `go-frog-audit` or the CLI wizard.
- Changing the existing CSV columns or crawl semantics when features are off.

### 2.3 Product principles

- **Async by default.** A crawl is a job, not a tool call that blocks for minutes.
- **Small, orthogonal tools.** Prefer six clear tools over twenty narrow ones.
- **Compact responses.** Summaries and pagination beat dumping a 10,000-row crawl into one response.
- **Server-enforced limits.** Never trust the agent’s requested limits.
- **Explicit capability errors.** If HTML was not archived, say so and explain how to enable it.
- **No arbitrary paths.** Jobs own their artifacts; tools address them by job ID.

### 2.4 Primary use cases

| Use case | Tool flow |
|---|---|
| “Crawl this site and find broken pages” | `start_crawl` -> `get_job` -> `get_results(status_code=4xx)` |
| “Run an internal linking audit and find orphans” | `start_crawl(audit=true)` -> `get_job` -> `get_audit(summary/pages)` |
| “Find every page mentioning a phrase” | `start_crawl(archive_html=true)` -> `get_job` -> `search_pages(scope=text)` |
| “Compare strict vs inclusive link counts” | `start_crawl(audit=true)` -> `get_audit(profile=strict/inclusive)` |
| “Give me the CSV” | `get_artifact(kind=csv)` |
| “List my recent crawls” | `list_jobs` -> `get_job` |

---

## 3. What exists today

### 3.1 Runtime and packaging

| Item | Current state |
|---|---|
| Language | Go 1.26.2 (`go.mod`) |
| Binaries | `go-frog` (web/CLI), `go-frog-audit` (archive re-audit) |
| Web UI | Embedded via `//go:embed web/*` |
| CGO | Disabled; static binaries |
| Default listen address | `:8080` (or `--addr`) |
| CLI mode | `--cli` interactive wizard |
| Docker build | `golang:1.26-alpine` build stage; `debian:bookworm-slim` runtime |
| Docker port | `8080` inside; compose publishes `4374:8080` |
| Runtime working directory | Not set in the runtime stage, so relative `results/` resolves to `/results` |

Relevant files:

- `main.go` — flags, CLI wizard, settings struct.
- `server.go` — job manager, HTTP handlers, HTML fragments, artifact downloads.
- `spider.go` / `list.go` — crawl runners.
- `scrape.go` — Colly callbacks, page recording.
- `extract.go` — title/description/H1/image/robots extraction and keyword counting.
- `export.go` — CSV output and result naming.
- `page.go` — `Page` model.
- `internal/audit/` — archive writer, audit engine, filters, URL normalization.
- `internal/auditbundle/` — ZIP + XLSX bundle builder.
- `cmd/audit/main.go` — standalone archive auditor.

### 3.2 Current HTTP surface

| Method | Path | Input | Output | Notes |
|---|---|---|---|---|
| GET | `/` | — | HTML page | Embedded template; prefills audit filter lists |
| POST | `/api/crawl` | `multipart/form-data` | HTML job fragment | Starts a goroutine and returns immediately |
| GET | `/api/crawl/{id}` | — | HTML job fragment | HTMX polls every 500 ms while running |
| GET | `/api/crawl/{id}/download` | — | CSV | CSV is generated at request time from in-memory pages |
| GET | `/api/crawl/{id}/audit/bundle.zip` | — | ZIP | Strict audit bundle |
| GET | `/api/crawl/{id}/audit/inclusive/bundle.zip` | — | ZIP | Inclusive audit bundle |

Form fields accepted by `POST /api/crawl`:

| Field | Meaning |
|---|---|
| `mode` | `spider` or `list` |
| `startUrl` | Required for spider |
| `urls` | Newline-separated, required for list |
| `keywords` | Pipe-separated values (for example, `pricing` and `contact`) |
| `concurrency` | Positive integer; server defaults to 10 |
| `audit` | `1` enables the internal linking audit (spider only) |
| `audit_nav_substrings` | Newline/comma-separated nav filter list |
| `audit_exclude_substrings` | Newline/comma-separated widget exclusion list |
| `audit_filters_explicit` | `1` means the submitted lists replace defaults/config |

Important limitations of the current API for machine clients:

- Responses are HTML fragments, not JSON.
- There is no job-list endpoint.
- There is no auth, rate limit, or ownership.
- There is no `Content-Type: application/json` anywhere.
- `POST /api/crawl` returns before the job finishes; a machine client must poll.
- `/api/crawl/{id}/download` does **not** reject a still-running job; it will serve an empty/partial CSV. An MCP tool must guard against that.

### 3.3 Current job model

`jobManager` is a mutex-protected `map[string]*crawlJob`:

```go
type jobManager struct {
    mu   sync.RWMutex
    jobs map[string]*crawlJob
}

type crawlJob struct {
    id        string
    mode      string // "spider" | "list"
    target    string
    createdAt time.Time

    mu         sync.Mutex
    status     string // "running" | "done" | "error"
    pages      int
    currentURL string
    errMsg     string
    result     []Page
    settings   WizardSettings

    auditReady   bool
    auditError   string
    auditSummary auditpkg.AuditSummary
    auditDir     string

    auditInclusiveReady   bool
    auditInclusiveError   string
    auditInclusiveSummary auditpkg.AuditSummary
    auditInclusiveDir     string
}
```

Behavior that matters for MCP:

- **IDs:** `newJobID()` generates 6 random bytes, hex-encoded (12 characters, 48 bits).
- **Statuses:** only `running`, `done`, `error`. No `queued`, `cancelled`, or phase.
- **Results:** `result` is set only when the crawl finishes. There is no partial result store.
- **Progress:** `pages` increments per completed request (including some connection-level errors); it is not necessarily `len(result)` during a run.
- **Audit:** the job is marked `done` even if the audit fails; `auditError` is set separately. Inclusive failure is independent.
- **TTL:** a goroutine ticks every 5 minutes and deletes every job whose `createdAt` is older than 30 minutes — **including jobs that are still running**.
- **Persistence:** none. A server restart loses every job.
- **Concurrency:** unbounded; every accepted request starts a goroutine immediately.
- **Ownership:** none. Any client that knows an ID can read it.
- **Result access:** the full `[]Page` stays in memory, so large crawls consume memory until the reaper runs.

### 3.4 Current crawl pipeline

#### Spider mode

`runSpider`:

1. Normalizes the start URL (adds `https://` if missing, strips fragment).
2. Treats apex and `www` as one site; subdomains are external.
3. Creates a Colly collector with async mode and `Parallelism = concurrency`.
4. Uses a 60-second HTTP client timeout.
5. Does **not** auto-follow redirects; 3xx responses become their own rows, and the `Location` target is visited separately if it is same-site.
6. Follows `a[href]` links on the same site.
7. Records every response through `attachPageRecording`.
8. Optionally creates an audit archive and discovers `robots.txt` / sitemaps **before** the crawl.
9. When the audit is on, fetches sitemap-only pages after link-following finishes.
10. Closes the archive, which writes `manifest.json` and `sitemap.json`.

There are **no maximum page, depth, URL, or duration limits** in the crawler.

Colly’s default `robots.txt` handling applies; the app does not set `IgnoreRobotsTxt`. A blocked URL may be skipped and show up only as a progress error, not as a page row. MCP tools should document this so agents do not mistake a robots block for a missing page.

#### List mode

`runList`:

1. Visits each URL once.
2. Does not follow links.
3. Does not track inlinks.
4. Does not create an audit archive.
5. Audit is not available in list mode; the UI disables it and the server ignores it.

#### Page record

```go
type Page struct {
    URL         string
    StatusCode  int
    LoadTime    time.Duration
    ParentURL   string
    Inlinks     string // serialized: fromURL>"anchor" | ...
    Title       string
    Description string
    H1s         []string
    ImgAlts     string // serialized: imgURL>"alt" | ...
    Robots      string
    KeywordHits []int
}
```

`Inlinks` and `ImgAlts` are serialized strings, not arrays. An MCP DTO can either expose them raw or parse them into structured arrays. Parsing is nicer for agents but needs careful handling of anchors containing delimiters (`"`, `|`, `>`); the existing format escapes parts. A pragmatic v1 can expose `inlinks_raw` / `img_alts_raw` and add structured fields later. See §10.2.

### 3.5 Current keyword search

`parseSearchKeywords` splits `KeywordsRaw` on `|`, trims, and drops empty values.

`keywordHitCounts` lowercases the raw HTML and counts literal substring occurrences per keyword. Consequences:

- Counts include markup, attributes, scripts, and inline JSON.
- Counts are case-insensitive but not Unicode-normalized.
- Non-200 responses get zero counts and blank metadata.
- Any 200 response body is counted, including non-HTML responses.
- There are no snippets, page text, or match positions.
- Keywords must be chosen before the crawl.

CSV output adds one `Search: <keyword>` column per keyword. `KeywordHits` is aligned to the parsed keyword order.

### 3.6 Current internal linking audit

The audit is opt-in via `audit=1` and spider-only.

When enabled, the archive writer stores:

```text
results/<timestamp>-<host>/
├── manifest.json          # every fetched URL, status, headers, depth, discovery source
├── sitemap.json           # discovered sitemaps/pages/warnings (when available)
├── html/
│   └── <sha256(url)[:20]>.html
├── audit.json
├── audit-pages.csv
├── audit-links.csv
├── audit-blocks.csv
└── inclusive/
    ├── audit.json
    ├── audit-pages.csv
    ├── audit-links.csv
    └── audit-blocks.csv
```

The archive writer saves HTML bodies only for 200 HTML responses. All other responses are metadata-only. `manifest.json` entries include URL, status, content type, location, redirect URL, `X-Robots-Tag`, depth, body file, body bytes, discovery source (`start`, `link`, `redirect`, `sitemap`), and fetch time.

`auditpkg.Analyze(archiveDir, Options)`:

- Reads the archive; never fetches the network.
- Produces a `Result` with pages, links, blocks, anchors, structure, redirects, failed pages, PageRank, sitemap metadata, and a summary.
- Writes `audit.json`, `audit-pages.csv`, `audit-links.csv`, and `audit-blocks.csv`.
- Runs twice in the web flow: strict and inclusive.

Strict vs inclusive:

- **Strict** excludes known widget/listing classes (related posts, carousels, sliders, etc.) and nav/menu/footer patterns.
- **Inclusive** keeps the same nav/menu/footer filtering but removes widget/listing exclusions.
- Both count only content page links: internal links whose normalized target is a saved 200 HTML page. Assets are omitted; redirect aliases, external links, and non-page targets are excluded.

Key data detail for MCP:

- `Result.Links` contains **content page links only**.
- The full link inventory (external, non-HTTP, redirect, excluded rows) is in `audit-links.csv`, not in `Result.Links`.
- `Result.Pages` is a `map[string]*Page`; sort keys for stable pagination.
- `Result.Blocks` contains every classified block, including excluded ones.

The web job stores only the **summary** and output directory for each profile. It does **not** keep the full `Result` in memory. An MCP audit tool must read `audit.json` from the job’s audit directory or the code must retain the result.

The audit bundle (`internal/auditbundle`) contains:

- `audit.xlsx` (Summary, Pages, Links, Blocks sheets)
- `audit-pages.csv`
- `audit-links.csv`
- `audit-blocks.csv`
- `audit.json`
- `manifest.txt` (row counts, **not** the crawl manifest)

### 3.7 Audit configuration precedence

The auditor looks for overrides in this order:

1. `--config <path>` (standalone `go-frog-audit` only)
2. `GOFROG_AUDIT_CONFIG`
3. `audit-config.json` inside the archive
4. `audit-config/<host>.json` in the current working directory

Overrides include `force_content`, `force_exclude`, `exclude_classes`, `nav_substrings`, `replace_exclude_classes`, and `replace_nav_substrings`.

The web UI sends `nav_substrings` and `exclude_substrings` with `explicit=1` when the user submits the filter editor. If `explicit` is false, the auditor uses defaults plus config.

For MCP, global env/config is risky when multiple agents audit different sites. A per-job inline override object is preferable; see §13.6.

### 3.8 Deployment and auth today

- The Go app has no authentication or authorization.
- A supported deployment puts oauth2-proxy in front of the human UI; the checked-in `compose.yml` does not.
- The checked-in `compose.yml` publishes `4374:8080` on all interfaces, with no volume and no proxy.
- Results and audit archives are inside the container and are lost when the container is recreated.
- Because the runtime image has no `WORKDIR`, relative `results/` resolves to `/results`; mount a volume there if you want artifacts to survive.

---

## 4. Gap analysis

| Capability | Today | Needed for MCP | Difficulty |
|---|---|---|---|
| JSON API | HTML fragments | JSON DTOs, pagination, errors | Low–medium |
| Job discovery | None | `list_jobs` + manager method | Low |
| Job status | Basic status/pages/current URL | Phase, timestamps, limits, truncation, artifacts | Low |
| Results | In-memory `[]Page` after finish | Filtering, sorting, pagination, keyword map | Low–medium |
| Search | Crawl-time keyword counts | Post-hoc archive search with snippets | Medium |
| Audit access | Summary + files | Sectioned, paginated JSON access | Medium |
| Artifacts | Unauthenticated download URLs | Auth or signed URLs, inline limits | Medium |
| AuthN | None | Bearer token or OAuth | Low–medium |
| AuthZ | Global jobs | Owner/scopes | Medium |
| Limits | None | Pages, depth, URLs, jobs, duration, response size | Medium |
| Persistence | None | TTL + optional job directory | Medium |
| Cancellation | None | Best-effort cancel + context | Medium–high |
| Observability | `fmt.Printf` logs | Structured logs, job events, metrics | Low–medium |
| Tests | Good crawl/audit baseline | MCP protocol, auth, limits, search | Medium |

---

## 5. MCP background

This section is deliberately short; it exists to anchor the design decisions.

### 5.1 What MCP is

The Model Context Protocol is a JSON-RPC 2.0 protocol between an **MCP client** (the agent host) and an **MCP server** (this app). It defines:

- **Tools:** model-controlled functions with JSON schemas.
- **Resources:** application-driven data identified by URIs.
- **Prompts:** user-controlled prompt templates.
- **Notifications:** progress, cancellation, and list changes.

For go-frog, tools are the primary surface. Resources are a nice-to-have for artifacts.

### 5.2 Transports

| Transport | Use | Notes |
|---|---|---|
| **stdio** | Local agent launches the server as a child process | Universal client support; logs must go to stderr |
| **Streamable HTTP** | Remote/hosted server | Standard modern remote transport; POST + optional SSE; session header |
| **Legacy SSE** | Old clients | Avoid for new work unless a target client requires it |

### 5.3 Tool results

A tool result can contain:

- `content`: text, image, audio, resource links, or embedded resources.
- `structuredContent`: a JSON object matching an optional `outputSchema`.
- `isError`: true for domain/tool errors (as opposed to JSON-RPC protocol errors).

This design uses `structuredContent` for machine-readable output and a short `text` summary for humans/agents that only read text.

### 5.4 Long-running work

MCP supports progress notifications and request cancellation, but client support varies. The reliable pattern is:

1. `start_crawl` returns a job ID immediately.
2. The agent polls `get_job` with backoff.
3. Optionally, the server emits progress notifications when the client supplied a progress token.

Do not model a multi-minute crawl as one blocking tool call.

Newer MCP revisions may expose long-running requests as **tasks**. If the pinned SDK and the target clients support tasks, a crawl job can be mapped onto one. Polling is the portable baseline and should be implemented first; tasks are an optimization, not a requirement.

### 5.5 Authorization

The MCP authorization spec uses OAuth 2.1 for HTTP transports. For a first version, a static bearer token is simpler and works with most clients. Treat OAuth as a later step; see §14.2.

### 5.6 SDK choice

- Official Go SDK: `github.com/modelcontextprotocol/go-sdk` (package `mcp`).
- Community alternative: `github.com/mark3labs/mcp-go`.

Both handle JSON-RPC, tool schemas, stdio, and Streamable HTTP. Pin a version and isolate SDK-specific types behind a small adapter so a protocol revision does not leak into the crawl/audit code.

> SDK API names and supported spec revisions change. Verify against the pinned version before implementation.

---

## 6. Architecture options

### 6.1 Option A — Embedded Streamable HTTP MCP (recommended)

```text
┌──────────────────────┐        Streamable HTTP         ┌─────────────────────────────┐
│ Agent / MCP client   │ ─────────────────────────────> │ go-frog process             │
│ (Claude, Cursor, …)  │ <───────────────────────────── │                             │
└──────────────────────┘        JSON-RPC / SSE         │  /mcp handler               │
                                                       │      │                      │
                                                       │      ▼                      │
                                                       │  jobManager (shared)        │
                                                       │      │                      │
                                                       │      ├── runSpider()        │
                                                       │      ├── runList()         │
                                                       │      └── auditpkg.Analyze() │
                                                       │                             │
                                                       │  web UI (/ and /api/crawl)  │
                                                       └─────────────────────────────┘
```

**Pros**

- Jobs started by agents are the same jobs the app already knows about.
- One process, one deployment, one set of limits.
- The web UI can later list agent-started jobs.
- No duplicate crawl/audit implementation.

**Cons**

- The MCP endpoint must be secured independently of the human UI.
- A single process is a single point of failure and does not scale horizontally without shared state.

### 6.2 Option B — stdio subprocess

```text
Agent host
  └── spawns: go-frog --mcp
        ├── MCP over stdin/stdout
        └── its own jobManager + crawl/audit
```

**Pros**

- Easiest to demo; no network/auth.
- Works with every MCP client that supports stdio.
- Good for a single user on their own machine.

**Cons**

- Separate process means separate jobs; work does not appear in the running web app.
- Results live and die with the agent session.
- Each agent/user duplicates crawl resources.
- Not suitable for a shared hosted service.

### 6.3 Option C — Separate MCP sidecar over the current HTTP API

```text
Agent --> MCP sidecar --> POST /api/crawl (multipart) --> HTML fragment
                       --> GET  /api/crawl/{id}       --> HTML fragment
```

**Why not**

- The API returns HTML fragments, not JSON.
- Job IDs and status would have to be scraped from markup.
- Audit artifacts are separate endpoints with no ownership.
- Two processes, duplicated config, duplicated limits.

This is only viable after adding a proper JSON HTTP API, at which point embedding is simpler.

### 6.4 Option D — Hybrid

- Hosted go-frog exposes Streamable HTTP MCP.
- A local stdio bridge (small separate binary or `go-frog --mcp-bridge`) proxies MCP calls to the remote server for clients that cannot do remote HTTP.

This is useful later, but not v1. The bridge is a thin HTTP client; it should not duplicate crawl logic.

### 6.5 Recommendation

| Scenario | Recommendation |
|---|---|
| One user, local machine | Option B (stdio) is enough |
| Team, hosted app, agents anywhere | Option A (embedded HTTP) |
| Legacy client without remote HTTP | Option D later |
| Quick demo | Option B first, then move the same tool handlers behind HTTP |

Design the tool handlers so they do not care which transport calls them.

---

## 7. Recommended architecture

### 7.1 Process and listeners

Keep one process. Create the `jobManager` once and pass it to both the web handlers and the MCP handlers.

Two listener choices:

1. **Same listener, path `/mcp`.** Simplest. Add the MCP handler to the existing `http.ServeMux`. Auth middleware applies only to `/mcp`. The reverse proxy routes the path.
2. **Separate listener, e.g. `127.0.0.1:8081`.** Better isolation: the human UI and machine API can have different auth, rate limits, and exposure. The same process and `jobManager` are shared.

**Recommendation:** start with the same listener and a dedicated `/mcp` auth middleware behind TLS. If the deployment already uses oauth2-proxy for the UI, add a separate listener so MCP traffic does not depend on browser cookies. Both are small changes; pick based on deployment.

### 7.2 Proposed components

| Component | Responsibility |
|---|---|
| `mcp.go` | Server construction, transport, lifecycle |
| `mcp_tools.go` | Tool registration and handlers |
| `mcp_types.go` | Input/output DTOs, pagination, error envelopes |
| `mcp_auth.go` | Bearer token parsing, scopes, owner context |
| `mcp_limits.go` | Quotas, rate limits, semaphores |
| `mcp_search.go` | Archive-backed string search |
| `mcp_audit.go` | Audit JSON loading/sectioning |
| `mcp_artifacts.go` | CSV/ZIP generation and signed URLs |
| `job_runner.go` (optional) | Interface around `runSpider`/`runList` for tests |

Keeping these in package `main` is pragmatic for v1 because `Page`, `WizardSettings`, and the job manager live there. Moving them into `internal/` is cleaner long-term but requires a type migration; do it only if the MCP layer grows.

### 7.3 Shared job manager changes

Current `startServer` creates the manager internally. Extract that creation into a small `newApp()` (or equivalent) so the web listener and the MCP listener share the same `*jobManager`.

Add to `jobManager`:

```go
func (jm *jobManager) list(owner string, filter jobFilter) []jobSummary
func (jm *jobManager) runningCount(owner string) int
func (jm *jobManager) createWithOwner(owner string, mode, target string, s WizardSettings) (*crawlJob, error)
func (jm *jobManager) delete(id string)
```

Add to `crawlJob`:

```go
phase      string     // starting, discovering_sitemap, crawling, auditing, done, ...
startedAt  time.Time
finishedAt time.Time
owner      string     // token ID, not the token
cancel     context.CancelFunc
limits     jobLimits
truncated  bool
```

Do not JSON-tag `crawlJob` directly. Create a separate `jobStatus` DTO so the HTML view and the MCP view can evolve independently.

### 7.4 Request flow

#### Start

```text
Agent                    MCP handler                  jobManager / runner
  │                          │                              │
  │ tools/call start_crawl   │                              │
  │─────────────────────────>│                              │
  │                          │ validate input               │
  │                          │ check auth/scopes/limits     │
  │                          │─────────────────────────────>│ create job
  │                          │<─────────────────────────────│ job ID
  │                          │ launch goroutine             │
  │<─────────────────────────│ {job_id, status, poll_ms}    │
  │                          │                              │
  │                          │                              ├── crawl
  │                          │                              ├── audit
  │                          │                              └── store result
```

#### Poll

```text
Agent                    MCP handler                  jobManager
  │ tools/call get_job       │                              │
  │─────────────────────────>│─────────────────────────────>│ snapshot
  │<─────────────────────────│ status, phase, pages, …      │
```

#### Retrieve

```text
Agent                    MCP handler                  job / archive
  │ tools/call get_results   │                              │
  │─────────────────────────>│ require done                 │
  │                          │ filter/sort/paginate         │
  │<─────────────────────────│ rows + total + has_more      │
```

#### Audit artifact

```text
Agent                    MCP handler                  filesystem
  │ tools/call get_audit     │                              │
  │─────────────────────────>│ read audit.json              │
  │                          │─────────────────────────────>│
  │<─────────────────────────│ section rows + pagination    │
```

### 7.5 stdio mode

Add a `--mcp` flag that:

- starts the MCP server on stdin/stdout;
- logs only to stderr;
- creates its own job manager;
- does not start the web server (unless explicitly requested);
- uses the same tool handlers as HTTP.

Critical: current code prints to stdout in `startServer` and the CLI. In stdio mode, **nothing** may write to stdout except protocol messages. Audit all `fmt.Print*` calls on that path. Progress bars already use stderr.

A future `--mcp-bridge <url>` can proxy to a hosted server for clients without remote HTTP; keep it separate from local job execution.

### 7.6 Graceful shutdown

Current `startServer` calls `http.ListenAndServe` with no shutdown handling. For MCP:

- trap `SIGINT` / `SIGTERM`;
- stop accepting new jobs;
- cancel or mark running jobs;
- close archive writers;
- persist job metadata if enabled;
- shut down the HTTP server with a timeout.

Without this, a container restart leaves half-written archives and orphaned temp files. If running two listeners, coordinate them with an `errgroup` or an error channel so one listener failing does not leave the process silently half-up.

### 7.7 Session and transport behavior

- **Initialize:** the server negotiates a protocol version and advertises capabilities (`tools`, optionally `resources`). Pin a tested revision and reject incompatible versions with a clear error.
- **Sessions:** Streamable HTTP may use an `Mcp-Session-Id` header. The SDK can manage sessions; if a stateless mode is chosen, document that progress notifications and resumability are unavailable.
- **Jobs are not sessions.** A job’s lifetime is independent of the MCP session. Reconnecting the client must not lose a running job, and closing the session must not cancel one.
- **Reconnection:** a client that reconnects can call `get_job` with the job ID. Do not require the original session to retrieve results.
- **Multiple replicas:** in-memory jobs are per process. Without shared state, a `get_job` request routed to another replica returns 404. For v1, run one replica. If scaling, use sticky routing by job ID/session or a shared job store.
- **Resumability:** optional SSE resumption is nice but not required for a polling design. Do not build v1 around it.

### 7.8 Anti-patterns to avoid

- **Blocking a tool call for the whole crawl.** Clients time out; use a job ID and polling.
- **Tying job lifetime to the MCP session or the `start_crawl` request.** Reconnects and tool-call cancellation must not kill a running job.
- **Using the job ID as authorization.** IDs are identifiers, not capabilities. Check ownership.
- **Returning the full CSV or `audit.json` inline.** Paginate or return a resource link.
- **Building a sidecar that scrapes the HTML UI/API.** Add a JSON/MCP layer in-process instead.
- **Trusting client-supplied limits.** Enforce page, depth, URL, duration, and response caps server-side.
- **Leaving artifact URLs unauthenticated.** That bypasses the MCP token.
- **Reaping running jobs.** Base TTL on completion and never evict active work.
- **Treating keyword counts as full-text search.** They are crawl-time raw-HTML counts, not snippets.
- **Running multiple replicas with an in-memory job manager.** Jobs will disappear behind a load balancer.

---

## 8. Tool catalog

The v1 surface is seven tools. Each tool has a stable name, a JSON input schema, a structured output, and a small text summary.

### 8.1 `start_crawl`

Starts a spider or list job. Returns immediately.

**Annotations:** `readOnlyHint: false`, `destructiveHint: false`, `idempotentHint: false`, `openWorldHint: true`.

Input fields:

| Field | Type | Required | Notes |
|---|---|---|---|
| `mode` | `"spider"` \| `"list"` | yes | Determines which URL field is used |
| `start_url` | string | spider | Normalized server-side; `https://` assumed if missing |
| `urls` | string[] | list | Deduplicated; max configurable |
| `keywords` | string[] | no | Max count/length enforced; a pipe character inside a keyword is rejected |
| `concurrency` | int | no | Default 10; capped by server |
| `audit` | bool | no | Spider only; enables archive + strict/inclusive reports |
| `audit_filters` | object | no | `nav_substrings`, `exclude_substrings`, `explicit` |
| `archive_html` | bool | no | Archive HTML even without audit; required for post-hoc search |
| `limits` | object | no | Per-job `max_pages`, `max_depth`, `max_duration_seconds`; cannot exceed server maxima |
| `client_request_id` | string | no | Idempotency key for retries |

Output:

```json
{
  "job_id": "8f14e45fceea167a5a36dedd4bea2543",
  "status": "running",
  "phase": "starting",
  "poll_after_ms": 1000,
  "audit": true,
  "archive_html": true,
  "links": {
    "self": "gofrog://jobs/8f14e45fceea167a5a36dedd4bea2543/status",
    "csv": "https://crawler.example.com/api/crawl/8f14e45fceea167a5a36dedd4bea2543/download",
    "audit_strict_zip": "https://crawler.example.com/api/crawl/8f14e45fceea167a5a36dedd4bea2543/audit/bundle.zip"
  }
}
```

Validation rules:

- Exactly one of `start_url` / `urls`, matching `mode`.
- `audit=true` with `mode=list` is an error.
- `urls` must be HTTP(S), deduplicated, and within the server's URL cap.
- `concurrency` must be 1..server max.
- `keywords` may be empty; each keyword must be non-empty and contain no `|`.
- `audit_filters.explicit=true` replaces defaults; if false, the object is ignored and defaults + config apply. Do not silently merge half-explicit lists.
- `archive_html=true` implies archive creation but not the audit report.
- Per-job limits cannot raise server maxima.

Errors: `invalid_argument`, `limit_exceeded`, `unauthorized`, `forbidden`, `conflict` (duplicate `client_request_id`).

### 8.2 `get_job`

Returns status/progress only. This is the polling endpoint.

**Annotations:** `readOnlyHint: true`, `idempotentHint: true`, `openWorldHint: false`.

Input: `job_id`.

Output:

```json
{
  "job_id": "8f14e45fceea167a5a36dedd4bea2543",
  "mode": "spider",
  "target": "https://example.com",
  "status": "running",
  "phase": "crawling",
  "created_at": "2026-11-10T12:00:00Z",
  "started_at": "2026-11-10T12:00:01Z",
  "finished_at": null,
  "pages_crawled": 128,
  "current_url": "https://example.com/blog/post",
  "keywords": ["pricing", "contact"],
  "truncated": false,
  "limits": {
    "max_pages": 5000,
    "max_depth": 10,
    "max_duration_seconds": 1800
  },
  "audit": {
    "enabled": true,
    "state": "pending",
    "strict": null,
    "inclusive": null
  },
  "error": null,
  "artifacts": {
    "csv_ready": false,
    "audit_strict_ready": false,
    "audit_inclusive_ready": false
  }
}
```

Rules:

- `pages_crawled` is the progress count, not necessarily the final result count.
- `current_url` may be empty before the first response or after completion.
- `keywords` lets the agent know which counts are present in `get_results`.
- `audit.state` is `disabled`, `pending`, `running`, `ready`, `error`, or `partial`.
- Audit failures do not make the crawl fail; represent them separately.

Errors: `job_not_found`, `unauthorized`, `forbidden`.

### 8.3 `list_jobs`

Lists recent jobs visible to the caller.

**Annotations:** `readOnlyHint: true`, `idempotentHint: true`, `openWorldHint: false`.

Input: `status` (optional), `mode` (optional), `limit` (default 20, max 100), `offset`.

Output: array of compact job summaries (same fields as `get_job` minus the long fields), plus `total`, `has_more`.

Rules:

- Filter by owner unless the token has `admin`.
- Sort by `created_at` descending.
- Do not expose tokens or proxy credentials.

### 8.4 `get_results`

Returns paginated page rows for a completed job.

**Annotations:** `readOnlyHint: true`, `idempotentHint: true`, `openWorldHint: false`.

Input:

| Field | Type | Notes |
|---|---|---|
| `job_id` | string | Required |
| `status_code` | int or int[] | Exact status filter |
| `url_contains` | string | Case-insensitive substring |
| `title_contains` | string | Case-insensitive substring |
| `keyword` | string | Must be one of the job’s keywords |
| `min_keyword_hits` | int | Used with `keyword` |
| `sort` | enum | `url`, `status_code`, `load_time`, `keyword_hits` |
| `order` | `asc` \| `desc` | Default `asc` for URL, `desc` for numeric |
| `offset` | int | Default 0 |
| `limit` | int | Default 100, max 500 |
| `include_audit` | bool | Join audit metrics by normalized URL when a strict audit is ready |

Output:

```json
{
  "job_id": "8f14e45fceea167a5a36dedd4bea2543",
  "total": 1234,
  "offset": 0,
  "limit": 100,
  "has_more": true,
  "summary": {
    "pages": 1234,
    "by_status": {"200": 1200, "301": 20, "404": 14},
    "keywords": ["pricing", "contact"],
    "truncated": false
  },
  "rows": [
    {
      "url": "https://example.com/about",
      "status_code": 200,
      "load_time_ms": 143,
      "parent_url": "https://example.com/",
      "inlinks_raw": "https://example.com/>\"Home\"",
      "title": "About",
      "description": "About the company",
      "h1s": ["About us"],
      "img_alts_raw": "https://example.com/logo.png>\"Logo\"",
      "robots": "index, follow",
      "keywords": {"pricing": 0, "contact": 2},
      "depth": 1,
      "discovered_via": "link"
    }
  ]
}
```

Rules:

- Require `status == "done"`; return `job_not_finished` otherwise. Do not silently return partial rows.
- `depth` and `discovered_via` are available only when an archive exists (`audit=true` or `archive_html=true`); otherwise `null` / omitted. Read them from `manifest.json`.
- `keywords` maps names to counts using the job’s parsed keyword order.
- `inlinks_raw` / `img_alts_raw` preserve the current serialization. Add parsed arrays later if agents need them.
- `include_audit=true` adds a nested `audit` object per row (`page_key`, content inbound/outbound, PageRank, orphan flags, reachability, sitemap flags). It requires a ready strict audit. Join by normalized URL; do not assume a raw string match. See §13.4.
- Compute `total` after filtering, before pagination.
- Cap response bytes as well as row count.

### 8.5 `search_pages`

Searches crawled page content for a string or regular expression.

**Annotations:** `readOnlyHint: true`, `idempotentHint: true`, `openWorldHint: false`.

Input:

| Field | Type | Notes |
|---|---|---|
| `job_id` | string | Required |
| `pattern` | string | Literal by default; regex if `regex=true` |
| `regex` | bool | Go RE2 syntax |
| `case_sensitive` | bool | Default false |
| `scope` | enum[] | `metadata`, `html`, `text`, `all`; default `metadata` |
| `context_chars` | int | Default 120, max 500 |
| `offset` | int | Match offset for pagination |
| `limit` | int | Default 50, max 200 |
| `max_matches_per_page` | int | Optional cap |

Output:

```json
{
  "job_id": "8f14e45fceea167a5a36dedd4bea2543",
  "pattern": "affiliate disclosure",
  "regex": false,
  "scope": ["text"],
  "total_matches": 37,
  "offset": 0,
  "limit": 50,
  "has_more": false,
  "truncated": false,
  "matches": [
    {
      "url": "https://example.com/blog/post",
      "field": "text",
      "count": 2,
      "snippets": [
        {
          "before": "…this post contains an ",
          "match": "affiliate disclosure",
          "after": " at the top…"
        }
      ]
    }
  ]
}
```

Rules:

- `scope=html` or `text` requires an archive. If the job has no archive, return `search_not_available` with a message explaining `audit=true` or `archive_html=true`.
- Without an archive, `scope=metadata` can search URL, title, description, H1s, robots, and serialized inlinks from `[]Page`.
- `regex=true` uses Go’s `regexp` (RE2): linear-time, no catastrophic backtracking. Still cap pattern length and match count.
- Literal search is case-insensitive by default; use `strings.EqualFold`/lowercase carefully for Unicode.
- Snippets from `html` are raw HTML context and may contain entities/tags. `text` strips tags via a parser (goquery) before searching.
- Pagination is by match, not by page; a page may contribute multiple matches.
- Stop scanning at the server cap and set `truncated`.

### 8.6 `get_audit`

Returns a section of the strict or inclusive audit report.

**Annotations:** `readOnlyHint: true`, `idempotentHint: true`, `openWorldHint: false`.

Input:

| Field | Type | Notes |
|---|---|---|
| `job_id` | string | Required |
| `profile` | `"strict"` \| `"inclusive"` | Default `strict` |
| `section` | enum | See below |
| `offset` | int | Default 0 |
| `limit` | int | Default 100, max 500 |
| `sort` | enum | Section-specific |
| `filter` | object | Section-specific, e.g. `decision`, `orphan_only`, `status` |

Sections:

| Section | Contents |
|---|---|
| `summary` | `AuditSummary` + high-level graph counts |
| `pages` | Paginated `Result.Pages`, sorted by URL, inbound, PageRank, or orphan status |
| `links` | Content page links from `Result.Links` |
| `blocks` | `Result.Blocks`, including excluded decisions |
| `anchors` | `Result.Anchors` aggregates |
| `pagerank` | `Result.PageRankRanking` + `PageRankMeta` |
| `redirects` | `Result.Redirects` |
| `failed` | `Result.Failed` |
| `sitemap` | `Result.Sitemap` |
| `structure` | `Result.Structure` |
| `all` | Summary + counts only; do not dump every array |

Rules:

- Require `status == "done"`.
- If audit is not enabled or not ready, return `audit_not_available`.
- If the inclusive profile failed, return `audit_not_available` with the stored error, not a generic failure.
- Read `audit.json` from the job-owned directory. Cache by path + mtime if needed.
- `links` returns **content page links only**. For external/redirect/excluded rows, use the `audit-links.csv` artifact.
- `pages` is a map; sort keys deterministically before pagination.
- For `all`, return counts and section availability, not full arrays.

### 8.7 `get_artifact`

Returns metadata for a downloadable artifact, or small inline content.

**Annotations:** `readOnlyHint: true`, `idempotentHint: true`, `openWorldHint: false`.

Input:

| Field | Type | Notes |
|---|---|---|
| `job_id` | string | Required |
| `kind` | enum | `csv`, `audit_strict_zip`, `audit_inclusive_zip`, `audit_json_strict`, `audit_json_inclusive` |
| `inline` | bool | Return content inline if small |

Output:

```json
{
  "job_id": "8f14e45fceea167a5a36dedd4bea2543",
  "kind": "csv",
  "filename": "2026-11-10-12-00-00-example.com.csv",
  "content_type": "text/csv",
  "size_bytes": 12345,
  "url": "https://crawler.example.com/api/crawl/8f14e45fceea167a5a36dedd4bea2543/download",
  "expires_at": "2026-11-10T12:15:00Z",
  "inline": false
}
```

Rules:

- Require `done`.
- CSV is generated at request time from `[]Page`, as today.
- Audit ZIPs are built with `auditbundle.WriteFile`; cache per job/profile if repeated requests are expected.
- Prefer a URL/resource link for large files. Inline only below a configurable byte cap (e.g., 1 MB).
- Artifact URLs must be authenticated. Either require the bearer token on the download endpoint or issue a short-lived HMAC-signed URL. Do not leave the existing unauthenticated download endpoint exposed on the MCP listener.
- Never accept an arbitrary path.

### 8.8 `cancel_job` (phase 2)

Best-effort cancellation of a running job.

**Annotations:** `readOnlyHint: false`, `destructiveHint: false`, `idempotentHint: true`, `openWorldHint: false`.

Input: `job_id`.

Output: `{job_id, status, phase, cancelled: true, note}`.

Implementation caveat: Colly has no simple global stop. A cancellation flag checked before enqueuing and in callbacks stops new work, but in-flight requests finish up to the 60-second client timeout. For v1, prefer a max duration/page cap and omit cancellation.

Important semantics: cancelling the **MCP tool call** (`notifications/cancelled`) after `start_crawl` returns should not automatically cancel the crawl. The job has its own lifetime. Only `cancel_job` cancels a job.

---

## 9. Resource catalog

Resources are optional. If the client supports them, they are a clean way to expose artifacts. If not, `get_artifact` works.

| URI | MIME type | Contents |
|---|---|---|
| `gofrog://jobs/{id}/status` | `application/json` | Job status (same as `get_job`) |
| `gofrog://jobs/{id}/results.csv` | `text/csv` | CSV report |
| `gofrog://jobs/{id}/audit/strict.json` | `application/json` | Strict `audit.json` |
| `gofrog://jobs/{id}/audit/inclusive.json` | `application/json` | Inclusive `audit.json` |
| `gofrog://jobs/{id}/audit/strict.zip` | `application/zip` | Strict bundle |
| `gofrog://jobs/{id}/audit/inclusive.zip` | `application/zip` | Inclusive bundle |

Rules:

- Resources are scoped to the authenticated owner.
- Do not expose raw filesystem paths.
- Large resources should be resource links, not embedded blobs.
- Resource templates should be listed only when the client supports them; otherwise tools are sufficient.

---

## 10. Data contracts

All JSON uses `snake_case`. Timestamps are RFC 3339 UTC. Durations are integer milliseconds unless the field name says otherwise. URLs are absolute unless labeled raw.

### 10.1 Job status

```json
{
  "job_id": "8f14e45fceea167a5a36dedd4bea2543",
  "mode": "spider",
  "target": "https://example.com",
  "status": "running",
  "phase": "crawling",
  "created_at": "2026-11-10T12:00:00Z",
  "started_at": "2026-11-10T12:00:01Z",
  "finished_at": null,
  "pages_crawled": 128,
  "current_url": "https://example.com/blog/post",
  "keywords": ["pricing", "contact"],
  "truncated": false,
  "limits": {
    "max_pages": 5000,
    "max_depth": 10,
    "max_duration_seconds": 1800
  },
  "audit": {
    "enabled": true,
    "state": "pending",
    "strict": null,
    "inclusive": null
  },
  "error": null,
  "artifacts": {
    "csv_ready": false,
    "audit_strict_ready": false,
    "audit_inclusive_ready": false
  }
}
```

`status` values: `queued`, `running`, `done`, `error`, `cancelled`.

`phase` values:

- `queued`
- `starting`
- `discovering_sitemap` (audit only)
- `crawling`
- `sitemap_coverage` (audit only)
- `auditing_strict`
- `auditing_inclusive`
- `done`
- `error`
- `cancelled`

The existing web UI can continue to treat `status == "running"` as active; `phase` is additive.

### 10.2 Page row

```json
{
  "url": "https://example.com/about",
  "status_code": 200,
  "load_time_ms": 143,
  "parent_url": "https://example.com/",
  "inlinks_raw": "https://example.com/>\"Home\"",
  "title": "About",
  "description": "About the company",
  "h1s": ["About us"],
  "img_alts_raw": "https://example.com/logo.png>\"Logo\"",
  "robots": "index, follow",
  "keywords": {"pricing": 0, "contact": 2},
  "depth": 1,
  "discovered_via": "link"
}
```

Field notes:

- `load_time_ms` converts `Page.LoadTime`.
- `parent_url` is empty in list mode.
- `keywords` is built from `KeywordHits` and the parsed keyword list.
- `depth` / `discovered_via` require an archive. If absent, omit the fields rather than returning misleading zeros.
- If structured inlinks are added later, keep `inlinks_raw` for compatibility and add `inlinks: [{from, anchor}]`.

### 10.3 Pagination envelope

```json
{
  "total": 1234,
  "offset": 0,
  "limit": 100,
  "has_more": true,
  "next_offset": 100,
  "truncated": false
}
```

Rules:

- `total` is the filtered count, not the unfiltered count.
- `has_more` is `offset + len(rows) < total`.
- `truncated` means the server stopped scanning/collecting at a cap, even if `has_more` is true/false. These are different conditions:
  - `has_more`: more rows exist and can be fetched.
  - `truncated`: the server deliberately stopped at a safety cap.
- Include `next_offset` only when `has_more`.

### 10.4 Tool error

Domain/tool errors use `isError: true` and a structured payload:

```json
{
  "code": "search_not_available",
  "message": "This job has no archived HTML. Start the crawl with audit=true or archive_html=true.",
  "retryable": false,
  "details": {
    "job_id": "8f14e45fceea167a5a36dedd4bea2543"
  }
}
```

Error codes:

| Code | Meaning | Typical HTTP analogue |
|---|---|---|
| `invalid_argument` | Bad input; message names the field | 400 |
| `unauthorized` | Missing/invalid token | 401 |
| `forbidden` | Token lacks scope or does not own the job | 403 |
| `job_not_found` | Unknown ID or reaped | 404 |
| `job_not_finished` | Results/artifacts requested too early | 409 |
| `audit_not_available` | Audit disabled, failed, or not ready | 409 |
| `search_not_available` | No archive or unsupported scope | 409 |
| `limit_exceeded` | Quota/rate/page limit | 429 |
| `conflict` | Duplicate idempotency key with different input | 409 |
| `internal_error` | Unexpected server failure | 500 |

Do not leak stack traces, filesystem paths, proxy credentials, or tokens in error messages.

### 10.5 Units and conventions

| Item | Convention |
|---|---|
| Timestamps | RFC 3339 UTC |
| Durations | Integer milliseconds (`load_time_ms`, `duration_ms`) |
| Job TTL | Seconds in config, rendered as RFC 3339 expiry in output |
| Bytes | `size_bytes` |
| URLs | Absolute; raw serialized values explicitly labeled `_raw` |
| Booleans | `true` / `false`, never `"1"` |
| Empty values | `null` for “not applicable”, `[]` for “known empty” |

### 10.6 Schema and versioning

- Keep tool names stable. Add fields; do not change the meaning of an existing field.
- Prefer additive JSON fields. Mark deprecated fields in the description and keep them for at least one release.
- Expose the server version via MCP `serverInfo`. Include the go-frog build version and the MCP schema/tool version in logs.
- Keep `ArchiveVersion` and `AuditVersion` independent of the MCP schema version; they version different artifacts.
- Validate inputs server-side even though the JSON Schema describes them. The schema is a hint, not a security boundary.
- If a breaking change is unavoidable, add a new tool name (`get_results_v2`) rather than silently changing the old one.
- Document the tested MCP protocol revision and client matrix in the release notes.

---

## 11. Job lifecycle changes

### 11.1 Phases

```text
queued
  │
  ▼
starting
  │
  ├── (audit) discovering_sitemap
  │
  ▼
crawling
  │
  ├── (audit) sitemap_coverage
  │
  ├── (audit) auditing_strict
  │
  ├── (audit) auditing_inclusive
  │
  ▼
done

Any active phase ──> error
Any active phase ──> cancelled (phase 2)
```

For list mode, skip sitemap/audit phases.

Why a phase field:

- An audit can run for a long time after crawling stops. Without a phase, agents see “running” with a frozen page count.
- It makes logs and UI clearer.
- It lets `get_job` tell the agent what to expect.

### 11.2 Timestamps

Add `startedAt` and `finishedAt`. Keep `createdAt` for compatibility.

- `startedAt` is set when the goroutine actually starts.
- `finishedAt` is set on `done`, `error`, or `cancelled`.
- TTL should be based on `finishedAt`, not `createdAt`.

### 11.3 Retention and TTL

Current behavior:

- Reaper ticks every 5 minutes.
- Deletes jobs older than 30 minutes by `createdAt`.
- Deletes running jobs too.

Proposed:

- Default TTL: **24 hours after completion**.
- Never reap `running` / `queued` jobs.
- Evict the oldest finished jobs when a retained-job cap is exceeded.
- Make TTL and cap configurable.
- On startup, if persistence is enabled, mark previously running jobs as `error` with code `interrupted`.

Why 24 hours: agent sessions and human review often happen hours after a crawl; 30 minutes is too short. An in-memory default is acceptable for v1 if documented.

### 11.4 Persistence options

| Option | Description | Recommendation |
|---|---|---|
| A. In-memory only | Jobs disappear on restart; TTL 24h | v1 |
| B. Per-job directory | `results/jobs/<id>/job.json`, `pages.json.gz`, artifacts | v1.5 |
| C. SQLite/bbolt | Indexed job metadata, retention, search | v2, only if needed |

Per-job directory sketch:

```text
results/jobs/<job_id>/
├── job.json          # settings, status, timestamps, summaries, limits
├── pages.json.gz     # page rows (optional; CSV can be regenerated)
├── report.csv        # generated CSV
└── audit/            # strict/inclusive report directories
```

Use atomic writes (temp file + rename), as the archive writer already does. Do not write partial JSON. If `pages.json.gz` is too large, store the CSV and parse it lazily, or rehydrate only summaries.

Docker note: mount a volume at `/results` so job data survives container recreation. The current compose file has no volume.

### 11.5 Queuing and concurrency

Current behavior starts a goroutine immediately. Proposed:

- A per-token semaphore (`max_running_jobs_per_token`, default 2).
- A global semaphore (`max_running_jobs`, default 4–8).
- If full, either queue up to a small limit or return `limit_exceeded` with `retry_after_ms`.
- Recommendation: queue up to 2× the global limit, then reject. Agents can retry.
- Never allow one token to starve others; fair queuing is nice but not required for v1.

### 11.6 Cancellation

Distinguish:

1. **MCP request cancellation** — the client cancels a `tools/call` request. For `start_crawl`, the job should keep running.
2. **Job cancellation** — explicit `cancel_job`, or server shutdown.

Implementation:

- Add a `context.Context` to the runner (`runSpider` / `runList`).
- Check `ctx.Err()` before enqueuing new URLs and in callbacks.
- Colly has no global stop; in-flight requests finish up to the 60-second client timeout.
- Add a max duration and page cap as the practical guardrail for v1.
- Mark the job `cancelled` and preserve partial results if useful.

### 11.7 Progress notifications

If the client supplies `_meta.progressToken`:

- Emit `notifications/progress` with `progress` (pages crawled) and `message` (current phase/URL).
- Throttle to at most 1 per second.
- Do not block the crawl on notification delivery; use a buffered channel and drop/merge updates if full.
- If the client did not supply a token, do nothing.

Polling via `get_job` remains the reliable baseline.

### 11.8 Idempotency

Agents retry after timeouts. Add optional `client_request_id`:

- Store a map `owner + client_request_id -> job_id` for the retention window.
- If the same key arrives with identical input, return the existing job.
- If the same key arrives with different input, return `conflict`.
- If no key is supplied, create a new job (current behavior).

This prevents duplicate crawls when an agent times out after `start_crawl` succeeded.

### 11.9 Multiple replicas

The current job manager is process-local. If go-frog runs behind a load balancer with more than one replica:

- a job created on replica A is invisible to replica B;
- a `get_job` request may 404 even though the job is running;
- Streamable HTTP sessions may also require affinity;
- artifact files may exist on only one replica.

Options:

| Option | Cost | Notes |
|---|---|---|
| Single replica | None | Recommended for v1 |
| Sticky routing by job ID/session | Low | Requires proxy support; does not survive replica loss |
| Shared job store (Redis/SQLite) | Medium | Jobs, results, ownership, TTL |
| External queue + workers | High | Best for scale; overkill for this app |

Do not claim horizontal scalability until jobs and artifacts are shared.

### 11.10 Limit semantics

Limits are easy to add and easy to get subtly wrong. Define them precisely:

| Limit | Applies to | Notes |
|---|---|---|
| `max_pages` | Distinct recorded URLs | Count page rows, not progress events; include sitemap-only pages |
| `max_depth` | Link-following depth | Sitemap-coverage pages are fetched outside the link frontier; give them a separate cap or treat them as depth 0 |
| `max_urls` | List-mode input | Deduplicate before counting |
| `max_duration` | Whole job | Include strict + inclusive audit time |
| `max_concurrency` | In-flight HTTP requests | Per job; also cap global request load |

When a limit is reached:

- Stop scheduling new work.
- Set `truncated=true` and a `limit_reason` (`max_pages`, `max_depth`, `max_duration`).
- Prefer finishing with partial results (`status=done`) over a hard `error`, because partial results are useful to an agent. Include a warning in `get_job`.
- If the audit archive is incomplete, mark `audit.state=partial` or skip the audit. Do not present a partial archive as a complete audit.
- Never silently truncate and report success without the flag.

---

## 12. Search design

### 12.1 Two different features

“Search for strings” can mean two things:

| Feature | When | What it returns |
|---|---|---|
| **Keyword counts** | Configured at job start | Per-page occurrence counts, no snippets |
| **Post-hoc search** | After the crawl | Matching pages, snippets, counts |

The existing `keywords` field covers the first. The second needs archived HTML or a text index.

### 12.2 Archive requirement

Today HTML is saved only when `audit=true` and only in spider mode. That is too narrow for general search.

Proposed `archive_html` flag:

- Independent of `audit`.
- Works in spider and list mode.
- Creates the same `ArchiveWriter` layout (`manifest.json`, `html/`), with `mode` set to `spider` or `list`.
- `audit=true` implies `archive_html=true`.
- Default false (storage cost).

Without an archive:

- `search_pages` can search metadata: URL, title, description, H1s, robots, and serialized inlinks.
- It cannot search body text.
- Return `search_not_available` for `html`/`text` scopes with a clear hint.

### 12.3 Search algorithm

For each manifest entry with a `BodyFile`:

1. Read the HTML body (bounded by a max body size).
2. For `text` scope, parse with goquery and extract visible text.
3. For `html` scope, search the raw body.
4. Find matches (literal or RE2 regex).
5. Collect up to `context_chars` before/after each match.
6. Normalize whitespace in snippets; optionally strip HTML tags for `text`.
7. Stop at the configured page/match cap and set `truncated`.

Optimizations:

- Use a worker pool of 2–4 readers; disk I/O is the bottleneck.
- Cache a lowercased text version per page on first search if repeated searches are expected.
- For very large archives, build a simple on-disk inverted index later; do not start with Bleve/Elasticsearch.

### 12.4 Semantics and limits

| Rule | Value |
|---|---|
| Default literal search | Case-insensitive |
| Regex engine | Go `regexp` (RE2) |
| Max pattern length | 512 characters |
| Max matches returned | `limit` (default 50, max 200) |
| Max context | 500 characters per side |
| Max pages scanned | Configurable (e.g., 10,000) |
| Max body size per page | Configurable (e.g., 5 MB) |
| Match count per page | Capped (e.g., 100) to avoid pathological pages |

Regex is safe from catastrophic backtracking because Go uses RE2. Still validate the pattern and cap input.

### 12.5 HTML vs text caveats

- Raw HTML search can match class names, scripts, JSON-LD, and attributes that a human never sees.
- Text extraction changes offsets and may merge words across tags.
- HTML entities (`&amp;`, `&nbsp;`) mean an exact phrase may not match raw HTML.
- Case-insensitive Unicode matching is approximate; document it.

For SEO use cases, `text` is usually what the agent wants. Offer `html` for debugging/markup audits.

---

## 13. Audit design

### 13.1 Access model

- The audit exists only for spider jobs with `audit=true`.
- Strict and inclusive reports are separate:
  - strict: `<auditDir>/audit.json`
  - inclusive: `<auditDir>/inclusive/audit.json`
- The job stores summaries and output dirs, not the full `Result`.
- `get_audit` reads `audit.json` from the job-owned directory and caches it.

### 13.2 Sections and pagination

`Result` contains large maps/slices. Do not return the entire thing.

| Section | Pagination | Sort options |
|---|---|---|
| `summary` | no | — |
| `pages` | yes | `url`, `content_inbound`, `pagerank`, `orphan` |
| `links` | yes | `source`, `target`, `anchor` |
| `blocks` | yes | `link_count`, `page_count`, `decision` |
| `anchors` | aggregates | fixed order |
| `pagerank` | yes | `pagerank`, `inbound` |
| `redirects` | yes | `url` |
| `failed` | yes | `url` |
| `sitemap` | bounded list | — |
| `structure` | no | — |
| `all` | summary + counts | — |

Filters:

- `pages`: `orphan_only`, `content_unreachable_only`, `sitemap_only`, `status_code`.
- `links`: `decision`, `reason`, `source_contains`, `target_contains`.
- `blocks`: `decision`, `reason`, `repeated_only`.
- `pagerank`: `min_inbound`.

### 13.3 Link inventory caveat

`Result.Links` is content page links only. If an agent asks “show me every link the audit considered,” the answer is in `audit-links.csv`, not `Result.Links`.

Options:

1. Return `links` as content links and document the caveat.
2. Add a `link_inventory` section that parses `audit-links.csv` into rows.
3. Add a dedicated `get_link_inventory` tool.

Recommendation for v1: option 1 + `get_artifact(kind=audit_*_zip)`. Add option 2 if agents need structured external/redirect rows.

### 13.4 URL identity

`Result.Pages` is a map keyed by the audit’s **graph identity**, not necessarily the crawl row’s requested URL. The audit normalizes to `https://<apex-host><path>` with no trailing slash, no `www.`, no fragment, and tracking parameters removed. `Result.Page` also carries `URL`, `RawURL`, and `FinalURL` fields.

Consequences for MCP:

- Do not assume `get_results().rows[i].url == get_audit(pages).key`.
- Expose the map key as `page_key` alongside `url`, `raw_url`, and `final_url`.
- Document that joining crawl rows to audit rows may require normalization.
- A `www` page and an apex page can collapse into one audit node; this is intentional.

### 13.5 Re-auditing an existing archive

The CLI supports:

```bash
go-frog-audit --archive results/2026-01-02-03-04-05-example.com
```

Do **not** expose an MCP tool that accepts an arbitrary filesystem path. If re-audit is needed:

- accept a `job_id` whose archive already exists;
- or accept an archive name restricted to the `results/` directory and validate it;
- never accept `../`, absolute paths, or symlink escapes.

### 13.6 Audit config for multiple sites

Global `GOFROG_AUDIT_CONFIG` is fine for one site, dangerous for many. A per-job override object is better:

```json
{
  "force_content": [".related-posts"],
  "force_exclude": [".newsletter"],
  "exclude_classes": ["my-widget"],
  "nav_substrings": ["my-chrome"],
  "replace_exclude_classes": false,
  "replace_nav_substrings": false
}
```

This requires `auditpkg.Analyze` to accept inline overrides (or a temporary config file). The current `Options` only has `ConfigPath`; add an `Overrides *Overrides` field or resolve the config before calling. Keep path-based config for the CLI.

Do not let MCP callers point at arbitrary config files.

### 13.7 Audit failures

Current web behavior:

- Crawl succeeds, audit fails -> job `status=done`, `auditError` set.
- Inclusive can fail independently.

MCP should mirror this:

```json
{
  "status": "done",
  "audit": {
    "enabled": true,
    "state": "partial",
    "strict": {"ready": true, "summary": {…}},
    "inclusive": {"ready": false, "error": "..."}
  }
}
```

`get_audit(profile=inclusive)` returns `audit_not_available` with the stored error.

---

## 14. Security

This is the part that determines whether the endpoint can be exposed beyond localhost.

### 14.1 Threat model

| Threat | Impact | Mitigation |
|---|---|---|
| Unauthenticated job creation | Resource exhaustion, internal scanning | Bearer token / OAuth |
| SSRF | Access to internal services/metadata endpoints | Egress policy, private-IP blocking, allowlist |
| Runaway crawl | CPU/memory/bandwidth exhaustion | Max pages, depth, URLs, duration, concurrency |
| Many concurrent jobs | Memory/disk exhaustion | Global/per-token semaphores |
| Result leakage | Cross-user data exposure | Job ownership + scopes |
| Artifact path traversal | Arbitrary file read | Job-owned dirs only; existing `ReadBody` guard |
| Regex DoS | CPU exhaustion | RE2 + pattern/match caps |
| Oversized input | Memory exhaustion | Body/URL/keyword/list caps |
| Token leakage | Full access | Env/file secret, never log, rotate |
| DNS rebinding | Bypass network restrictions | Validate resolved IPs; Host/Origin checks |
| CORS abuse | Browser-based unauthorized access | Allowlist origins; bearer tokens |
| Log injection/PII | Sensitive data in logs | Structured logs, redaction policy |

### 14.2 Authentication

**v1: static bearer token.**

- `Authorization: Bearer <token>`.
- Token from `GOFROG_MCP_TOKEN` or `GOFROG_MCP_TOKEN_FILE`.
- Compare with `crypto/subtle.ConstantTimeCompare` against a stored hash.
- Token ID for logs: first 8 hex chars of SHA-256; never log the token.
- If no token is configured:
  - refuse to start the HTTP MCP listener unless the listener is loopback **and** `--mcp-allow-anonymous` is explicitly set;
  - log a loud warning.

**Scopes (optional but recommended):**

| Scope | Allows |
|---|---|
| `crawl:start` | `start_crawl`, `cancel_job` |
| `crawl:read` | `get_job`, `list_jobs`, `get_results`, `search_pages` |
| `audit:read` | `get_audit`, audit artifacts |
| `artifact:read` | `get_artifact` |
| `admin` | All jobs, all tokens |

For a single-team deployment, one token with all scopes is fine.

**OAuth 2.1 (later):**

- Useful for third-party clients and per-user consent.
- Requires protected-resource metadata, authorization server discovery, token validation, and client registration support.
- Do not block v1 on it.

### 14.3 Authorization and ownership

- Tag each job with the token ID that created it.
- `get_job`, `get_results`, `get_audit`, `search_pages`, `get_artifact` check ownership.
- `list_jobs` filters by owner unless `admin`.
- Do not rely on unguessable IDs for authorization. Job IDs are currently 48 bits; use 128 bits for new jobs.
- UI-created jobs have no MCP owner. Treat them as a separate `ui` owner (or admin-only) so an agent token cannot enumerate human jobs by default.
- If the human UI is separate, decide whether UI users can see agent jobs. Default: yes for admins, no for other tokens.

### 14.4 Network exposure

- Bind MCP to `127.0.0.1` by default.
- Terminate TLS at a reverse proxy; do not expose plain HTTP over a network.
- If using the same listener as the UI, apply auth only to `/mcp` and keep the UI’s existing proxy auth.
- If using a separate listener, expose only that port through the proxy and keep the UI port private.
- Validate `Host` and `Origin` to reduce DNS rebinding risk, especially on loopback.
- CORS: disabled by default. If a browser-based agent needs it, allowlist exact origins and permit `Authorization`.
- CSRF: bearer tokens in the `Authorization` header are not sent automatically by browsers, so classic CSRF is not the primary risk. If cookie auth is added, require `SameSite` cookies and a CSRF token.

### 14.5 SSRF and egress policy

The crawler already fetches arbitrary URLs. MCP makes that programmatic and potentially multi-tenant.

Add an egress policy for MCP-created jobs (and optionally all jobs):

1. Parse the URL; allow only `http`/`https`.
2. Resolve the hostname.
3. Reject:
   - loopback (`127.0.0.0/8`, `::1`),
   - private (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`),
   - link-local (`169.254/16`, `fe80::/10`),
   - unspecified (`0.0.0.0`, `::`),
   - multicast,
   - IPv4-mapped IPv6 variants,
   - cloud metadata addresses (`169.254.169.254`).
4. Apply the same check to:
   - redirect `Location` targets,
   - sitemap URLs,
   - list-mode URLs.
5. Prefer a custom `DialContext` that validates the resolved IP and dials the validated address, to reduce DNS rebinding.
6. Optional allowlist: `GOFROG_ALLOWED_HOSTS` with exact and `*.suffix` matches. When set, deny everything else.
7. Make private-network crawling a deliberate opt-in (`GOFROG_ALLOW_PRIVATE_NETWORKS=true`) for internal deployments.

Caveat: blocking private IPs breaks crawling internal/staging sites. Document the trade-off.

### 14.6 Resource limits

Defaults to consider (tune after load tests):

| Limit | Default | Notes |
|---|---|---|
| `max_pages` per spider job | 5,000 | 0 = unlimited only for trusted/admin |
| `max_depth` | 10 | Colly exposes request depth |
| `max_urls` per list job | 1,000 | Deduplicated |
| `max_concurrency` | 20 | UI currently allows 1–50; MCP should cap lower |
| `max_job_duration` | 30 minutes | Crawl + audit; return truncated/error at cap |
| `max_running_jobs` global | 4–8 | Semaphore |
| `max_running_jobs_per_token` | 2 | Fairness |
| `max_retained_jobs` | 100 | Evict oldest finished |
| `job_ttl` | 24 hours | After completion |
| `max_keywords` | 20 | Each max 100 chars |
| `max_pattern_length` | 512 | Search |
| `max_results_limit` | 500 | `get_results` |
| `max_audit_limit` | 500 | `get_audit` |
| `max_inline_artifact_bytes` | 1 MB | `get_artifact(inline=true)` |
| `max_request_bytes` | 1 MB | MCP JSON request body |
| `max_url_length` | 2,048 | Start/list URLs |
| `max_response_bytes` | e.g. 5 MB | Hard cap per tool result |
| `rate_limit` | e.g. 60 tool calls/min/token | Token bucket |

Enforce limits server-side, not just in the schema description.

### 14.7 Input validation

- URLs: HTTP(S), host present, length cap, no credentials in URL (or explicitly allow?), no fragments after normalization.
- Keywords: count/length caps, no `|`, no control characters.
- Regex: length cap, compile before scanning.
- Audit filters: count/length caps, normalized the same way as the web UI.
- Job IDs: exact-match lookup, no path use.
- Artifact kinds: enum whitelist.
- Pagination: non-negative offset, limit within max.
- Strings: reject invalid UTF-8? JSON already requires valid UTF-8. Trim, but do not silently alter user input beyond documented normalization.

### 14.8 Artifact access

The existing download endpoints are unauthenticated at the app level. If the MCP listener is separate and public, do not return those URLs directly.

Options:

1. Require the same bearer token on `/api/crawl/{id}/download` and audit bundle endpoints when accessed via the MCP listener.
2. Issue short-lived HMAC-signed URLs:
   `/api/crawl/{id}/download?expires=...&sig=...`
   Bind the signature to job ID, artifact kind, owner, and expiry.
3. Proxy artifact downloads through an MCP-authenticated endpoint.

Recommendation: (1) for simplicity, (2) if clients cannot send headers when fetching URLs. Never expose a permanent unauthenticated URL.

### 14.9 Data privacy and retention

- Crawl results can contain personal data, internal URLs, and page content.
- Set a retention policy (24h default) and delete artifacts on expiry.
- Redact query strings/tokens from logs where possible.
- Do not include full page bodies in error messages.
- If multiple users share the server, isolate results and document that the server operator can read everything.

### 14.10 Audit logging

Log:

- who (token ID),
- what (tool, mode, target host),
- when,
- job ID,
- outcome,
- pages/bytes,
- error code.

Do not log:

- the token,
- full page bodies,
- proxy credentials (`WizardSettings.ProxyURL` must never appear in MCP output or logs),
- sensitive query parameters (or redact them).

---

## 15. Configuration

### 15.1 Proposed flags

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--mcp` | — | off | Run MCP over stdio (local mode) |
| `--mcp-http` | `GOFROG_MCP_HTTP` | off | Enable MCP over HTTP |
| `--mcp-addr` | `GOFROG_MCP_ADDR` | `127.0.0.1:8081` | Separate MCP listener (if used) |
| `--mcp-path` | `GOFROG_MCP_PATH` | `/mcp` | HTTP path |
| `--mcp-token` | `GOFROG_MCP_TOKEN` | — | Bearer token (prefer file) |
| `--mcp-token-file` | `GOFROG_MCP_TOKEN_FILE` | — | File containing the token |
| `--mcp-allow-anonymous` | `GOFROG_MCP_ALLOW_ANONYMOUS` | false | Only valid on loopback |
| `--mcp-allowed-origins` | `GOFROG_MCP_ALLOWED_ORIGINS` | — | CORS allowlist |
| `--job-ttl` | `GOFROG_JOB_TTL` | `24h` | Retention after completion |
| `--max-pages` | `GOFROG_MAX_PAGES` | `5000` | Per spider job |
| `--max-depth` | `GOFROG_MAX_DEPTH` | `10` | Per spider job |
| `--max-list-urls` | `GOFROG_MAX_LIST_URLS` | `1000` | Per list job |
| `--max-concurrency` | `GOFROG_MAX_CONCURRENCY` | `20` | Server cap |
| `--max-job-duration` | `GOFROG_MAX_JOB_DURATION` | `30m` | Crawl + audit |
| `--max-jobs` | `GOFROG_MAX_JOBS` | `4` | Global running cap |
| `--max-jobs-per-token` | `GOFROG_MAX_JOBS_PER_TOKEN` | `2` | Fairness |
| `--archive-html` | `GOFROG_ARCHIVE_HTML` | false | Default archive behavior |
| `--allow-private-networks` | `GOFROG_ALLOW_PRIVATE_NETWORKS` | false | SSRF policy override |
| `--allowed-hosts` | `GOFROG_ALLOWED_HOSTS` | — | Egress allowlist |
| `--rate-limit` | `GOFROG_RATE_LIMIT` | `60/m` | Tool calls per token |
| `--artifact-base-url` | `GOFROG_ARTIFACT_BASE_URL` | — | Public URL for links |
| `--artifact-signing-key` | `GOFROG_ARTIFACT_SIGNING_KEY` | — | HMAC key for signed URLs |
| `--log-format` | `GOFROG_LOG_FORMAT` | `text` | `text` or `json` |

The current `main.go` parses flags manually. Adding this many flags is a good reason to switch to the standard `flag` package or a small config struct. Preserve `--cli` and `--addr`.

Two listener modes:

- **Mounted:** `--mcp-http` with no `--mcp-addr` mounts `/mcp` on the existing UI listener.
- **Separate:** `--mcp-http --mcp-addr 127.0.0.1:8081` starts a second listener in the same process.

Pick one deliberately. Running both without a reason doubles the exposure surface.

### 15.2 Example config file (optional)

If a config file is preferred over many env vars:

```json
{
  "mcp": {
    "http": true,
    "addr": "127.0.0.1:8081",
    "path": "/mcp",
    "token_file": "/run/secrets/gofrog_mcp_token",
    "allowed_origins": ["https://app.example.com"]
  },
  "jobs": {
    "ttl": "24h",
    "max_running": 4,
    "max_running_per_token": 2,
    "max_retained": 100
  },
  "limits": {
    "max_pages": 5000,
    "max_depth": 10,
    "max_list_urls": 1000,
    "max_concurrency": 20,
    "max_duration": "30m"
  },
  "egress": {
    "allow_private_networks": false,
    "allowed_hosts": []
  }
}
```

Do not add a config file format unless it is needed; env + flags are enough for v1.

### 15.3 Docker compose sketch

Illustrative only; adjust to the chosen listener model.

```yaml
services:
  go-frog:
    image: sge-go-frog-go-frog:latest
    ports:
      - "127.0.0.1:4374:8080"   # human UI
      - "127.0.0.1:4375:8081"   # MCP, behind a proxy
    volumes:
      - ./results:/results
    environment:
      GOFROG_MCP_HTTP: "true"
      # Bind 0.0.0.0 inside the container so Docker's port mapping can reach it.
      # The host-side mapping above keeps it on 127.0.0.1; the proxy exposes it.
      GOFROG_MCP_ADDR: "0.0.0.0:8081"
      GOFROG_MCP_TOKEN_FILE: "/run/secrets/gofrog_mcp_token"
      GOFROG_JOB_TTL: "24h"
      GOFROG_MAX_PAGES: "5000"
    secrets:
      - gofrog_mcp_token
    restart: unless-stopped

secrets:
  gofrog_mcp_token:
    file: ./gofrog_mcp_token
```

If MCP shares the main listener, drop the second port and add `GOFROG_MCP_PATH=/mcp`.

### 15.4 Reverse proxy notes

For Streamable HTTP:

- Disable response buffering for `/mcp` (SSE/long-lived responses).
- Set a generous read timeout.
- Forward `Authorization` and `Mcp-Session-Id`.
- Do not strip `Content-Type: application/json`.
- If multiple replicas are used, make sure sessions are sticky or the server is stateless.

Nginx sketch (adjust the upstream to the published host port if go-frog runs in Docker):

```nginx
location /mcp {
    proxy_pass http://127.0.0.1:4375;  # or 127.0.0.1:8081 for a host process
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Authorization $http_authorization;
    proxy_buffering off;
    proxy_read_timeout 3600s;
}
```

If the app binds `127.0.0.1` inside a container, Docker port publishing will not reach it; bind `0.0.0.0` inside the container and keep the host-side mapping on `127.0.0.1`.

---

## 16. Client setup

Client config formats vary. The examples below are illustrative; check the client’s current documentation.

### 16.1 Local stdio

```json
{
  "mcpServers": {
    "go-frog": {
      "command": "/usr/local/bin/go-frog",
      "args": ["--mcp"],
      "env": {
        "GOFROG_MAX_PAGES": "5000",
        "GOFROG_ARCHIVE_HTML": "true"
      }
    }
  }
}
```

Notes:

- The binary must be the full build, not `go-frog-audit`.
- `GOFROG_ARCHIVE_HTML=true` enables post-hoc search for local jobs.
- Jobs live in the child process; they do not appear in a separately running web UI.
- Logs go to stderr; stdout is protocol-only.

### 16.2 Remote Streamable HTTP

```json
{
  "mcpServers": {
    "go-frog": {
      "url": "https://crawler.example.com/mcp",
      "headers": {
        "Authorization": "Bearer <token>"
      }
    }
  }
}
```

Notes:

- Some clients use `"type": "http"` or a different top-level key.
- If the client cannot set headers, use a local stdio bridge or OAuth.
- The endpoint must be reachable through TLS; do not send tokens over plain HTTP.

### 16.3 Example agent workflow

```text
1. start_crawl
   mode=spider, start_url=https://example.com, audit=true,
   keywords=["pricing","contact"], archive_html=true
   -> job_id

2. get_job (poll every 1–2 s)
   -> phase=crawling, pages_crawled=…
   -> phase=auditing_strict, …
   -> status=done

3. get_results
   job_id, status_code=404, limit=100
   -> broken URLs

4. get_audit
   job_id, profile=strict, section=summary
   -> pages, content links, orphans, sitemap-only

5. get_audit
   job_id, profile=strict, section=pages,
   sort=content_inbound, order=asc, limit=50
   -> orphan candidates

6. search_pages
   job_id, pattern="affiliate disclosure", scope=["text"]
   -> pages with snippets

7. get_artifact
   job_id, kind=csv
   -> download URL
```

This is the core value proposition: an agent can run a full crawl + audit + content search without a human clicking through the UI.

---

## 17. Observability and operations

### 17.1 Logging

Use `log/slog` with a consistent event schema:

```json
{
  "ts": "2026-11-10T12:00:00Z",
  "level": "info",
  "event": "job_started",
  "job_id": "8f14e45fceea167a5a36dedd4bea2543",
  "token_id": "a1b2c3d4",
  "mode": "spider",
  "target_host": "example.com",
  "audit": true,
  "max_pages": 5000
}
```

Events:

- `mcp_initialized`
- `tool_called` (name, duration)
- `job_queued`, `job_started`, `job_phase`, `job_finished`, `job_failed`, `job_cancelled`
- `job_reaped`
- `artifact_built`
- `auth_failed`
- `limit_exceeded`

Do not log tokens or full page bodies.

### 17.2 Metrics (optional)

If a metrics endpoint is added:

- `gofrog_jobs_started_total{mode,audit}`
- `gofrog_jobs_finished_total{status}`
- `gofrog_job_duration_seconds{phase}`
- `gofrog_pages_crawled_total`
- `gofrog_active_jobs`
- `gofrog_mcp_tool_calls_total{tool,status}`
- `gofrog_mcp_tool_duration_seconds{tool}`
- `gofrog_artifact_bytes_total{kind}`
- `gofrog_auth_failures_total`

Avoid adding a Prometheus dependency unless needed; structured logs + `expvar` may be enough for v1.

### 17.3 Health checks

- `GET /healthz` — process alive.
- `GET /readyz` — results directory writable; job manager initialized; MCP token configured if HTTP MCP is enabled.
- Do not require an outbound network check.

### 17.4 Cleanup

- Reap expired jobs and their job directories.
- Delete orphaned temp files from interrupted bundle builds.
- Cap archive disk usage; reject new archive jobs when the cap is reached.
- Log cleanup failures.

### 17.5 Backups

If jobs are persisted:

- Back up `results/jobs/` and audit archives.
- Back up the token/config, not in the same location as results.
- Test restore.

---

## 18. Testing plan

### 18.1 Baseline

The repo already has:

- `server_test.go` — template and job fragment tests.
- `audit_integration_test.go`, `sitemap_integration_test.go` — crawl/audit integration.
- `internal/audit/*_test.go` — audit engine, filters, reachability, PageRank, URL handling.
- `internal/auditbundle/bundle_test.go` — bundle contents.

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
```

### 18.2 New test layers

| Layer | What to test | Approach |
|---|---|---|
| Unit | Input validation, DTO mapping, pagination, keyword map, error codes | Table-driven tests |
| Job manager | Create/list/get/reap, ownership, TTL, concurrency caps | In-memory manager, fake clock |
| Tool handlers | Each tool’s success/error paths | Fake runner + in-memory job manager |
| MCP protocol | initialize, tools/list, tools/call, structured content, errors | SDK client over in-memory/httptest transport |
| Integration | Start crawl against `httptest` site; poll; results; audit; search | Full process with temp `results/` |
| Auth | Missing/invalid token, scopes, ownership, admin | httptest requests |
| Limits | Max pages/depth/URLs, response caps, rate limit | Fake runner and synthetic data |
| Security | SSRF private IPs, DNS rebinding, path traversal, regex caps | httptest + custom dialer |
| Artifacts | CSV/ZIP correctness, auth, inline cap | Temp dirs, ZIP readers |
| Concurrency | Multiple jobs, race detector, reaper vs running job | `-race`, barriers |
| Load | 10k pages, 100 concurrent status calls | Synthetic `[]Page`, benchmark |
| Compatibility | stdio and HTTP, two clients, reconnect | Manual + automated smoke |

### 18.3 Testability refactor

Wrap the runners:

```go
type crawlRunner interface {
    Spider(ctx context.Context, s WizardSettings, onProgress func(string)) ([]Page, error)
    List(ctx context.Context, urls []string, s WizardSettings, onProgress func(string)) ([]Page, error)
}
```

The real implementation calls `runSpider` / `runList`; tests use a fake that returns controlled pages or blocks on a channel. This avoids network dependence and makes phase/limit tests deterministic.

### 18.4 MCP protocol tests

Use the SDK’s client against the server:

- initialize with a supported protocol version;
- list tools and verify names/schemas;
- call `start_crawl` with a fake runner;
- poll `get_job` until done;
- fetch `get_results` with filters/pagination;
- call `get_audit` with a synthetic `audit.json`;
- trigger errors and verify `isError` + structured code;
- verify auth rejection before any tool execution.

Golden tests for JSON schemas help catch accidental breaking changes.

### 18.5 Regression guarantees

- Existing web UI endpoints must behave exactly as before when MCP is disabled.
- Existing CSV columns must not change.
- Existing audit artifacts must not change.
- `go-frog-audit` CLI behavior must not change.
- New JSON fields are additive.

---

## 19. Rollout plan

### Phase 0 — Spike (0.5–1 day)

- Pick and pin an MCP Go SDK.
- Implement one tool (`get_job` or a trivial `ping`).
- Verify stdio and Streamable HTTP with one client.
- Confirm the SDK’s auth/session behavior.
- Decide same-listener vs separate-listener.

**Exit criteria:** a client can initialize and call one tool.

### Phase 1 — Core jobs (1–1.5 days)

- Add DTOs and pagination.
- Add `phase`, timestamps, `list_jobs`, owner field.
- Implement `start_crawl`, `get_job`, `list_jobs`, `get_results`.
- Fix reaper TTL (do not delete running jobs; 24h finished TTL).
- Add basic limits: max pages/depth/URLs, concurrency cap.
- Tests with a fake runner.

**Exit criteria:** agent can start a crawl and retrieve paginated results.

### Phase 2 — Remote access and safety (1–1.5 days)

- Add HTTP transport and config flags.
- Add bearer token auth, scopes, ownership.
- Add SSRF egress policy or explicitly document the risk and keep the endpoint loopback-only.
- Add rate limiting and global/per-token job caps.
- Add structured logging and health endpoints.

**Exit criteria:** endpoint is safe to expose behind TLS to a trusted agent.

### Phase 3 — Audit, search, artifacts (1–1.5 days)

- Implement `get_audit` sections and pagination.
- Add `archive_html` for post-hoc search.
- Implement `search_pages` for metadata and archived HTML/text.
- Implement `get_artifact` with CSV/ZIP URLs and inline limits.
- Add signed URLs or authenticated artifact endpoints.

**Exit criteria:** agent can run audit, inspect results, search content, and download artifacts.

### Phase 4 — Hardening (1–2 days)

- Persistence option (per-job directory) and Docker volume.
- Cancellation (best effort) or explicit non-support.
- Graceful shutdown.
- Metrics/logging polish.
- Docs, client configs, examples.
- Security review and load test.

**Exit criteria:** restart-safe (or clearly documented as ephemeral), tested with `-race`, documented limits.

---

## 20. Effort estimate

### 20.1 By workstream

| Workstream | Estimate | Notes |
|---|---|---|
| MCP SDK spike | 0.5–1 day | Protocol + transport + client smoke test |
| Tool handlers + schemas | 1–1.5 days | 5–7 tools, validation, errors |
| JSON DTOs + pagination | 0.5–1 day | Page rows, job status, audit sections |
| Job manager changes | 0.5–1 day | Phase, TTL, list, ownership, limits |
| HTTP transport + auth | 0.5–1 day | Token, scopes, host/origin checks |
| SSRF/egress + limits | 0.5–1 day | Resolver/dialer, quotas, rate limit |
| Audit tools | 0.5–1 day | Read/section/paginate `audit.json` |
| Search | 0.5–1.5 days | Archive scan, text extraction, snippets |
| Artifacts | 0.5–1 day | CSV/ZIP, auth/signed URLs, caching |
| Persistence | 0.5–1 day | Per-job directory, startup recovery |
| Tests | 1–2 days | Unit, integration, protocol, security |
| Docs/deployment | 0.5 day | Client configs, compose, runbook |

### 20.2 By scope

| Scope | Calendar estimate | What you get |
|---|---|---|
| Demo | 0.5–1 day | stdio, start/status, no auth, local only |
| Solid single-user v1 | 3–5 days | HTTP, auth, results, audit, search, limits, tests |
| Team deployment | 5–8 days | Ownership, quotas, persistence, observability |
| Multi-user production | 1–2 weeks | OAuth or per-user tokens, isolation, retention, load/security review |

Assumptions:

- One experienced Go developer.
- No major refactor of `Page` or the audit engine.
- MCP clients tested with a small matrix, not every client.
- Search is archive-scan based, not a full inverted index.
- No horizontal scaling.

Reconciliation: phases 0–3 cover the **3–5 day** single-user v1. Phase 4 (persistence, cancellation, metrics polish, full security/load review) is what pushes the total toward **5–8 days**; multi-user work pushes it to **1–2 weeks**. The phases can overlap, and the estimates are ranges, not commitments.

---

## 21. Risks and mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Runaway crawl from a confused agent | High | High | Max pages/depth/duration, global job cap |
| SSRF to internal services | Medium | High | Egress policy, private-IP block, allowlist |
| Token leakage | Low–medium | High | Env/file secret, no logs, rotation, TLS |
| Cross-user result leakage | Medium (multi-user) | High | Ownership checks, 128-bit IDs, scopes |
| Client cannot handle long jobs | High | Medium | Async job ID + polling; progress optional |
| MCP client cannot set auth headers | Medium | Medium | stdio bridge, OAuth, or proxy-injected auth |
| SDK/protocol churn | Medium | Medium | Pin version; isolate SDK behind adapter |
| Result too large for client | High | Medium | Pagination, compact summaries, byte caps |
| Audit JSON too large | High | Medium | Sections, pagination, `all` = summary only |
| Search scans too much HTML | Medium | Medium | Worker pool, page/match caps, truncation |
| Docker loses artifacts on restart | High | Medium | Volume at `/results`, persistence |
| Reaper deletes running job | High (current) | High | Fix TTL before enabling MCP |
| Multiple replicas + in-memory jobs | Medium | High | Single replica or shared store; sticky sessions |
| Colly cancellation is slow | Medium | Low–medium | Page/duration caps; document best effort |
| Global audit config wrong for some sites | Medium | Medium | Per-job inline overrides; explicit filters |
| Artifact URL bypasses auth | Medium | High | Authenticated downloads or signed URLs |
| Regex/HTML edge cases | Low | Low–medium | RE2, caps, text mode, tests |
| Logs leak sensitive URLs/PII | Medium | Medium | Redaction policy, structured logs |
| Job ID collision/guessing | Low | Medium | 128-bit IDs; ownership checks |

---

## 22. Open decisions

| Decision | Options | Recommendation |
|---|---|---|
| Transport | stdio only / HTTP only / both | Both; HTTP primary for hosted, stdio for local |
| Listener | Same as UI / separate port | Same `/mcp` for simplicity; separate port if UI uses oauth2-proxy |
| Auth | None / static token / OAuth 2.1 | Static bearer token v1; OAuth later |
| Ownership | Global / per-token | Per-token; admin can see all |
| Persistence | Memory / per-job dir / database | Memory v1; per-job dir v1.5; DB only if needed |
| Retention | 30 min / 24 h / configurable | 24 h after completion |
| Search | Keywords only / archive scan / index | Archive scan with caps; keywords always |
| Archive default | Off / on for audit / always on | Off by default; audit implies on |
| Cancellation | None / best effort / full | Max duration + caps v1; best effort later |
| Audit config | Global env / per-job inline | Per-job inline plus global defaults |
| Multi-user | Single team / multi-tenant | Single team v1; harden before multi-tenant |
| Replicas | One / many | One; shared store if scaling |
| Tool count | Minimal / comprehensive | 7 core tools; add only with a clear use case |
| MCP spec version | Latest / pinned | Pin a tested revision; upgrade deliberately |

---

## 23. Definition of done

A solid v1 is done when:

- [ ] An MCP client can initialize and list tools over the chosen transport.
- [ ] `start_crawl` starts spider and list jobs and returns a job ID immediately.
- [ ] `get_job` reports status, phase, pages, current URL, limits, and audit state.
- [ ] `list_jobs` shows only the caller’s jobs (unless admin).
- [ ] `get_results` supports filtering, sorting, and pagination, and refuses running jobs.
- [ ] `get_audit` returns summary, pages, links, blocks, anchors, PageRank, redirects, and failed sections.
- [ ] `search_pages` works for metadata without an archive and for HTML/text with an archive.
- [ ] `get_artifact` returns authenticated CSV/ZIP URLs, with an inline cap.
- [ ] Running jobs are never reaped; completed jobs expire after the configured TTL.
- [ ] Per-token and global job caps are enforced.
- [ ] Max pages/depth/URLs/duration and response size caps are enforced.
- [ ] Bearer token auth is required for HTTP; tokens are never logged.
- [ ] Private-IP/SSRF policy is applied, or the endpoint is loopback-only and documented as such.
- [ ] Existing web UI, CSV columns, audit artifacts, and CLI behavior are unchanged.
- [ ] `go test ./...`, `go test -race ./...`, and `go vet ./...` pass.
- [ ] Client config examples and deployment/runbook docs are updated.
- [ ] A restart either preserves completed jobs or is explicitly documented as ephemeral.
- [ ] Graceful shutdown does not leave half-written archives.

---

## 24. Appendices

### Appendix A — Current HTTP API details

#### `POST /api/crawl`

- Method: POST only; other methods return 405.
- Body: multipart form, max 10 MB (`ParseMultipartForm(10 << 20)`).
- Mode:
  - `spider`: `startUrl` required; `audit=1` optional.
  - `list`: `urls` required; lines parsed, deduplicated, invalid lines ignored; at least one valid URL required.
- `keywords`: trimmed; `|` separated.
- `concurrency`: parsed as positive int; otherwise default 10.
- Audit: only for spider and only when `audit=1`.
- Filters: applied only when `audit_filters_explicit=1`.
- Response: HTML job fragment with `Content-Type: text/html`; the job starts in a goroutine.

#### `GET /api/crawl/{id}`

- Returns the job fragment.
- While `status == "running"`, the fragment includes HTMX polling every 500 ms.
- Unknown ID: 404.

#### `GET /api/crawl/{id}/download`

- If `status == "error"`, returns 500 with the error text.
- Otherwise writes a temp CSV from `job.result` and serves it.
- Does not check `status == "done"`; a running job yields an empty/partial CSV.
- Filename: `results/<YYYY-MM-DD-HH-MM-SS>-<slug>.csv` generated at request time.
- Slug: spider host; list mode `CSVPath` is empty in the web flow, so `list`.

#### `GET /api/crawl/{id}/audit/bundle.zip`

- Requires `auditReady`; otherwise 404.
- Builds a temp ZIP with `auditbundle.WriteFile` and serves it.
- Filename: `<slug>-strict-audit.zip`.

#### `GET /api/crawl/{id}/audit/inclusive/bundle.zip`

- Same, for the inclusive profile.
- Filename: `<slug>-inclusive-audit.zip`.

### Appendix B — Current data structures

#### `WizardSettings`

```go
type WizardSettings struct {
    Mode        int // 1 = Spider, 2 = List
    StartURL    string
    CSVPath     string
    KeywordsRaw string
    Concurrency int
    ProxyURL    string

    Audit           bool
    AuditArchiveDir string
    AuditFilters    *auditpkg.FilterOptions

    listURLs []string
}
```

#### `Page`

See §3.4.

#### `AuditSummary`

```go
type AuditSummary struct {
    Pages                  int       `json:"pages"`
    ContentLinks           int       `json:"content_links"`
    ContentPageLinks       int       `json:"content_page_links"`
    ContentLinkOccurrences int       `json:"content_link_occurrences"`
    UniqueContentLinks     int       `json:"unique_content_links"`
    Orphans                int       `json:"orphans"`
    ContentOrphans         int       `json:"content_orphans"`
    ContentUnreachable     int       `json:"content_unreachable"`
    SitemapOnly            int       `json:"sitemap_only"`
    NavLinks               int       `json:"nav_links"`
    ExternalLinks          int       `json:"external_links"`
    SelfLinks              int       `json:"self_links"`
    GeneratedAt            time.Time `json:"generated_at"`
    OutDir                 string    `json:"out_dir"`
}
```

#### `Result` (shape)

```go
type Result struct {
    Version                int              `json:"version"`
    StartURL               string           `json:"start_url"`
    CanonicalRoot          string           `json:"canonical_root"`
    Domain                 string           `json:"domain"`
    CrawledAt              time.Time        `json:"crawled_at"`
    GeneratedAt            time.Time        `json:"generated_at"`
    ArchiveDir             string           `json:"archive_dir"`
    OutputDir              string           `json:"output_dir"`
    ConfigPath             string           `json:"config_path,omitempty"`
    FiltersUsed            *FilterOptions   `json:"filters_used,omitempty"`
    Pages                  map[string]*Page `json:"pages"`
    Links                  []Link           `json:"links"`
    ContentLinkOccurrences int              `json:"content_link_occurrences"`
    UniqueContentLinks     int              `json:"unique_content_links"`
    Orphans                []string         `json:"orphans"`
    ContentUnreachable     []string         `json:"content_unreachable,omitempty"`
    SitemapOnly            []string         `json:"sitemap_only,omitempty"`
    Sitemap                *SitemapMeta     `json:"sitemap,omitempty"`
    PageRankRanking        []RankEntry      `json:"pagerank_ranking"`
    PageRankMeta           PageRankMeta     `json:"pagerank_meta"`
    TopShare               float64          `json:"top_share"`
    Excluded               Excluded         `json:"excluded"`
    Blocks                 []Block          `json:"blocks"`
    Anchors                Anchors          `json:"anchors"`
    Structure              Structure        `json:"structure"`
    Redirects              []Redirect       `json:"redirects,omitempty"`
    DeadEnds               []string         `json:"dead_ends,omitempty"`
    Failed                 []Failed         `json:"failed,omitempty"`
    Summary                AuditSummary     `json:"summary"`
}
```

### Appendix C — Proposed file layout

Minimal, package `main`:

```text
mcp.go              # server construction, transport, lifecycle
mcp_tools.go        # tool registration/handlers
mcp_types.go        # DTOs, pagination, errors
mcp_auth.go         # bearer token, scopes, owner context
mcp_limits.go       # quotas, semaphore, rate limiter
mcp_search.go       # archive-backed search
mcp_audit.go        # audit JSON loading/sectioning
mcp_artifacts.go    # CSV/ZIP, signed URLs
mcp_test.go         # unit/integration tests
```

Long-term, if the layer grows:

```text
internal/mcp/
internal/jobs/
internal/model/
```

Do not move `Page`/`WizardSettings` until the MCP layer is stable; the migration cost is not worth it for v1.

### Appendix D — Example MCP JSON-RPC

#### Initialize

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": {
    "protocolVersion": "2025-06-18",
    "capabilities": {},
    "clientInfo": {"name": "example-agent", "version": "1.0.0"}
  }
}
```

#### List tools

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/list",
  "params": {}
}
```

#### Call `start_crawl`

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "method": "tools/call",
  "params": {
    "name": "start_crawl",
    "arguments": {
      "mode": "spider",
      "start_url": "https://example.com",
      "keywords": ["pricing", "contact"],
      "audit": true,
      "archive_html": true,
      "concurrency": 10
    }
  }
}
```

#### Tool result

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "Started spider job 8f14e45f... for https://example.com"
      }
    ],
    "structuredContent": {
      "job_id": "8f14e45fceea167a5a36dedd4bea2543",
      "status": "running",
      "phase": "starting",
      "poll_after_ms": 1000
    },
    "isError": false
  }
}
```

#### Error result

```json
{
  "jsonrpc": "2.0",
  "id": 4,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "This job has no archived HTML. Start the crawl with audit=true or archive_html=true."
      }
    ],
    "structuredContent": {
      "code": "search_not_available",
      "message": "This job has no archived HTML. Start the crawl with audit=true or archive_html=true.",
      "retryable": false
    },
    "isError": true
  }
}
```

### Appendix E — Compact `start_crawl` JSON Schema

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["mode"],
  "properties": {
    "mode": {"enum": ["spider", "list"]},
    "start_url": {"type": "string", "minLength": 1, "maxLength": 2048},
    "urls": {
      "type": "array",
      "items": {"type": "string", "maxLength": 2048},
      "maxItems": 1000
    },
    "keywords": {
      "type": "array",
      "items": {"type": "string", "minLength": 1, "maxLength": 100},
      "maxItems": 20
    },
    "concurrency": {"type": "integer", "minimum": 1, "maximum": 20},
    "audit": {"type": "boolean"},
    "archive_html": {"type": "boolean"},
    "audit_filters": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "nav_substrings": {"type": "array", "items": {"type": "string"}},
        "exclude_substrings": {"type": "array", "items": {"type": "string"}},
        "explicit": {"type": "boolean"}
      }
    },
    "limits": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "max_pages": {"type": "integer", "minimum": 1},
        "max_depth": {"type": "integer", "minimum": 0},
        "max_duration_seconds": {"type": "integer", "minimum": 1}
      }
    },
    "client_request_id": {"type": "string", "maxLength": 200}
  }
}
```

Validation still happens server-side; the schema is a hint, not the security boundary.

### Appendix F — Glossary

| Term | Meaning |
|---|---|
| **MCP** | Model Context Protocol; JSON-RPC between agent hosts and tools |
| **MCP client** | The agent host that connects to servers |
| **MCP server** | This app’s MCP surface |
| **Tool** | A model-callable function with an input schema |
| **Resource** | Application-provided data addressed by URI |
| **stdio** | Transport over stdin/stdout |
| **Streamable HTTP** | Modern HTTP transport for remote MCP |
| **SSE** | Server-sent events; legacy MCP transport |
| **JSON-RPC** | The wire protocol used by MCP |
| **SSRF** | Server-side request forgery |
| **RE2** | Go’s linear-time regex engine |
| **TTL** | Time to live; retention duration |
| **Strict audit** | Audit profile that excludes widget/listing classes |
| **Inclusive audit** | Audit profile that keeps widget/listing links as content |
| **Content page link** | Internal link whose target is a saved 200 HTML page |
| **Archive** | `manifest.json` + `html/` + audit reports for a crawl |
| **Artifact** | CSV, audit JSON, or audit ZIP produced by a job |

### Appendix G — References

- MCP specification: <https://modelcontextprotocol.io/specification>
- MCP authorization: <https://modelcontextprotocol.io/specification/draft/basic/authorization>
- Official Go SDK: <https://github.com/modelcontextprotocol/go-sdk>
- Community Go SDK: <https://github.com/mark3labs/mcp-go>
- Go `regexp` (RE2): <https://pkg.go.dev/regexp>
- OWASP SSRF prevention: <https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html>
- Existing app docs: `README.md`, `DEPLOYMENT.md`
- Existing audit specs: `specs/audit-refinements/`

---

## Summary

Adding MCP to go-frog is **not a rewrite**. The existing job manager, crawl runners, keyword counter, and audit engine already provide the hard behavior. The work is:

1. Wrap them in a small set of async, paginated JSON tools.
2. Make job lifecycle and retention suitable for agents.
3. Add archive-backed search.
4. Add auth, limits, and SSRF protection before exposing it.

If the goal is a local agent on the same machine, the shortest path is a `--mcp` stdio mode: roughly **half a day to a day** for a useful demo. If the goal is a hosted endpoint that any agent can call, budget **3–5 days** for a solid single-user v1 and **1–2 weeks** for a safe multi-user deployment.

The most important thing to fix before any public exposure is the 30-minute reaper deleting running jobs, followed by auth and page/depth caps. Those are small code changes with outsized operational impact.
