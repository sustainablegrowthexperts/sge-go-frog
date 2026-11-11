# go-frog

A web-based site crawler that follows links across a domain or visits a list of URLs, then exports a CSV report with page titles, status codes, redirects, and optional keyword counts. Built for SEO and content teams.

---

## Quick start (Docker)

```bash
docker compose up -d --build
```

Open **http://localhost:4374/**.

---

## Web UI

1. **Spider** — Enter a starting URL. go-frog follows internal links on the same domain and reports every page.
2. **List** — Paste URLs (one per line). Each URL is visited once — no crawling.
3. **Keywords** (optional) — Click **+ Add keyword** to add search terms. A column `Search: <keyword>` appears in the CSV with the count of occurrences in the raw HTML of each page.
4. **Concurrency** — Number of simultaneous requests. Default is 10.

Click **Run Crawl**. A job card shows progress in real time — page count, current URL, and a progress bar. When done, a **⬇ Download CSV** button appears.

### Internal linking audit (opt-in)

Tick **Perform internal linking audit?** in Spider mode. With the audit off, crawl behavior is unchanged. With it on, go-frog also reads `robots.txt` and the site sitemap (including sitemap indexes), then fetches sitemap pages that link-following could not reach. Those sitemap-only pages are normal crawl rows, so they also appear in the CSV and `manifest.json`. In addition, go-frog saves:

- `results/<run>/manifest.json` — every fetched URL, status, redirect target, content type, robots header, depth, discovery source (`start`, `link`, `redirect`, `sitemap`), and timestamp
- `results/<run>/sitemap.json` — fetched sitemap documents, discovered page URLs, and non-fatal sitemap warnings
- `results/<run>/html/*.html` — raw HTML for every HTTP 200 HTML page

After the crawl finishes, the deterministic auditor reads that archive and writes a **Strict** report plus an **Inclusive** companion:

- `audit.json` — content-link graph, block decisions, anchor analysis, and counts
- `audit-pages.csv` — per-page content inbound/outbound occurrences and unique values, nav inbound, PageRank, orphan flag/type, sitemap flags, and content/nav reachability
- `audit-links.csv` — every non-asset link the audit considered, including external, non-HTTP, redirect-target, and excluded rows, with raw and normalized targets/anchors, target type, decision, and reason; asset links are omitted entirely
- `audit-blocks.csv` — every link block with its evidence fields and classification decision/reason

The same four files are written under `inclusive/` inside the run archive. In the web UI, each profile is exposed as a single `*-audit.zip` download instead of four separate files. The bundle contains `audit.xlsx` (Summary, Pages, Links, and Blocks sheets with typed columns and filters), the three CSV files, `audit.json`, and `manifest.txt` with the row counts. The CSVs and JSON are copied unchanged, so existing scripts can still consume them.

Strict keeps the class-based widget exclusions; Inclusive uses the same navigation/menu/footer filters but removes every widget/listing exclusion, so related posts, recent-post blocks, carousels, sliders, product/blog listing grids, and similar links count as content. The two profiles differ only by the exclusion list. Both still exclude self-links, external and non-HTTP links, redirect aliases, non-page/404 targets, and assets. The job card shows **Strict** and **Inclusive** tabs with separate summaries and bundle downloads.

Both reports count **content page links only**: an internal link counts only when its normalized target is a saved 200 HTML page. Asset links (`.mp4`, `.pdf`, images, archives, and similar files) are omitted entirely — no rows, counts, anchors, or graph metrics. Redirect aliases and non-page/404 targets are excluded from the graph; the audit does not fetch, resolve, or health-check them. `Summary.ContentLinks`, `Summary.ContentPageLinks`, and `Summary.ContentLinkOccurrences` are content page link occurrences; `Summary.UniqueContentLinks` counts distinct source→target pairs. Header/navbar/footer/menu links are ranked as nav in both profiles. In the **Strict** report, related posts, editorial feeds, carousels, sliders, and motion widgets are **excluded** by class/id substring matching (`excluded_class:<match>`) and never contribute to graph metrics; repeating them does not change the decision. The **Inclusive** report removes those class exclusions while keeping the nav/menu/footer filters, and emits `filters_used.exclude_substrings` as an explicit empty array.

Block classification is **purely subtractive**: start with content, apply explicit `force_exclude` / `force_content` overrides, remove class-excluded widgets, remove nav/header/footer markers, and treat everything left as content. The auditor does not look for `<main>`/`<article>` content roots or use link-density scoring, so a link-dense block is never reclassified as nav solely because it sits outside a content container. `audit.json` block rows and `audit-blocks.csv` expose the reduced evidence schema: `nav_marker`, `excluded_class`, `position`, `link_count`, `link_text_ratio`, `direct_anchor_count`, `decision`, and `reason` (plus the existing block identity, sample, and snippet fields).

Redirect aliases are reported for visibility only. They do not pass link equity in this audit; link to the final URL if the link should count.

Reachability is computed by breadth-first search from the start page, not from direct inbound counts. **Content orphans** are non-start pages with zero content inbound; **content-unreachable** pages include direct orphans plus pages that are only reachable through another orphan. Nav reachability is a separate nav-only BFS. Pages found in the sitemap but not reachable through content links are flagged **sitemap-only, content-unreachable**. The audit uses the same site boundary as the crawl: apex and `www` are one site, subdomains are external. Inbound counts are link occurrences; anchor over-optimization is measured by distinct source pages. **PageRank uses unique source→target edges; page-table inbound/outbound columns are occurrences unless labeled unique.**

#### PageRank methodology

`audit.json` includes a `pagerank_meta` block that records the graph size and settings used for the ranking. The methodology is:

- **Nodes:** the saved HTTP 200 HTML pages in the archive (200 in the comparison run). Assets, redirect aliases, and non-page targets are not nodes. Strict also omits excluded widgets; Inclusive counts them.
- **Edges:** one per distinct `source -> target` content page pair. Repeated links between the same two pages count once.
- **Self-links:** excluded before PageRank is computed.
- **Dangling nodes:** their rank is redistributed uniformly across all nodes each iteration.
- **Damping, tolerance, iterations:** damping `0.85`, convergence tolerance `1e-8`, maximum `100` iterations.
- **Ranking metric:** PageRank over the content page graph. The page-table inbound/outbound counts are link occurrences unless the column is labeled unique.
- **`top_share`:** the raw top PageRank divided by the raw sum of all PageRank scores. PageRank values in the report are rounded to six decimals only for output; ordering and `top_share` use the unrounded values.

After the run, each audit tab downloads one ZIP bundle; the individual JSON and CSV files are no longer offered as separate web downloads. To re-audit the same archive later, or run it on another machine:

```bash
go-frog-audit --archive results/2026-01-02-03-04-05-example.com
```

Or with the Docker image:

```bash
docker run --rm -v "$PWD/results:/results" --entrypoint /go-frog-audit go-frog \
  --archive /results/2026-01-02-03-04-05-example.com
```

Optional per-site overrides (deterministic, no model):

```json
{
  "force_content": [".related-posts", "#more-from-us"],
  "force_exclude": [".newsletter", ".cookie-banner"],
  "exclude_classes": ["my-widget", "my-carousel"],
  "nav_substrings": ["my-chrome", "my-footer"],
  "replace_exclude_classes": false,
  "replace_nav_substrings": false
}
```

The auditor looks for overrides in this order: `--config <path>`, `GOFROG_AUDIT_CONFIG`, `audit-config.json` inside the archive, then `audit-config/<host>.json` in the current working directory. For the web UI, use `GOFROG_AUDIT_CONFIG` or `audit-config/<host>.json`; `force_content` / `force_exclude` match the block, anchor, or one of their ancestors and take priority over class exclusion.

`exclude_classes` and `nav_substrings` are case-insensitive partial class/id substrings (not CSS selectors), normalized for `_`/`-` and whitespace. They append to the built-in lists. Set `replace_exclude_classes` or `replace_nav_substrings` to `true` to replace the corresponding defaults instead, which is how a site can remove a broad default such as `stream`.

---

## MCP server (AI agents)

go-frog can expose an embedded Model Context Protocol (MCP) server over Streamable HTTP. An MCP client such as Claude can start spider or list crawls, poll them, read results and audits, search archived pages, and download artifacts without using the web UI. The MCP endpoint reuses the same crawl, job, audit, and artifact pipeline as the UI. MCP tools only see jobs created through MCP; web UI jobs stay browser-only.

### Enabling the endpoint

MCP is off by default. Enable it on the existing listener (port `8080` by default) and keep the default path:

```bash
GOFROG_MCP_ENABLED=true GOFROG_MCP_PATH=/mcp ./go-frog
```

The equivalent flags are `--mcp` and `--mcp-path`. When running under Docker, copy `.env.example` to `.env` (or edit `compose.yml`) and set at least:

```bash
GOFROG_MCP_ENABLED=true
GOFROG_MCP_PATH=/mcp
GOFROG_MCP_RESULTS_DIR=/results
GOFROG_MCP_PERSIST=true
```

See [DEPLOYMENT.md](DEPLOYMENT.md) for proxy routing, the results volume, restart behavior, and the operational runbook.

### Authentication

There is no app-level token validation, OAuth flow, or per-caller ownership. Authentication is enforced by the deployment's oauth2-proxy with one shared Bearer token: clients send `Authorization: Bearer <token>`, the proxy validates it, and go-frog assumes the request is already authenticated. Tokens are never logged. Use TLS at the proxy; do not send the token over plain HTTP.

### The eight tools

| Tool | What it does |
|------|--------------|
| `start_crawl` | Start an asynchronous spider or list crawl with optional keywords, audit, `archive_html`, per-job audit filters, per-job limits, and `client_request_id`. Returns a job ID immediately. |
| `get_job` | Poll a job for status, phase, page progress, current URL, limits, truncation, audit state, and artifact readiness. |
| `list_jobs` | List MCP-created jobs newest first, filtered by status or mode and paginated. |
| `get_results` | Read completed page rows with filtering, sorting, pagination, keyword counts, summary counts, manifest depth/discovery, and an optional strict-audit join. |
| `search_pages` | Search crawl metadata without an archive, or archived raw HTML and visible text when the job has an archive. Supports literal and RE2 regex patterns, case sensitivity, snippets, and pagination. |
| `get_audit` | Read a strict or inclusive audit section: summary, pages, links, blocks, anchors, pagerank, redirects, failed, sitemap, structure, or all. |
| `get_artifact` | Get an absolute download URL, size, filename, content type, and expiry for the CSV report, strict/inclusive audit ZIP, strict/inclusive audit JSON, or strict/inclusive audit XLSX. Small CSV and JSON bodies can be returned inline. |
| `read_artifact` | Read artifact bytes directly in the tool response when the client cannot fetch download URLs. CSV and audit JSON are UTF-8 text; ZIP and XLSX are base64. Supports byte-offset paging with `offset`, `limit`, and `next_offset`. |

### Job lifecycle and polling

`start_crawl` returns immediately; crawls run in the background. Polling with `get_job` is the v1 progress mechanism. Use `status` (`queued`, `running`, `done`, `error`, `cancelled`) to decide when to stop, and `phase` (`queued`, `starting`, `discovering_sitemap`, `crawling`, `sitemap_coverage`, `auditing_strict`, `auditing_inclusive`, `done`, `error`, `cancelled`) to see what the job is doing. `list_jobs` shows only jobs created by MCP tools. Reuse the same `client_request_id` to retry `start_crawl` safely: an identical request returns the existing job, while different input with the same ID returns a `conflict` error. `get_results`, `get_audit`, `get_artifact`, and `read_artifact` refuse jobs that are still running.

### Search, archives, and audits

`search_pages` can search page metadata without an archive. The HTML and text scopes need the saved page archive, so start the job with `audit=true` or `archive_html=true`. `get_audit` requires `audit=true`. If no archive exists, `search_pages` returns `search_not_available` with a hint; `get_audit` returns `audit_not_available`.

### Artifacts

`get_artifact` returns an absolute download URL, size, filename, content type, and expiry for one artifact. Set `inline=true` to return small CSV or JSON content directly, up to the 1 MB inline cap. Download URLs pass through the same oauth2-proxy as the MCP endpoint, so clients must send the same Bearer token when they fetch them.

`read_artifact` returns the artifact bytes in the tool response itself, so an MCP client behind an outbound network allowlist can retrieve reports without fetching the download URL. CSV and audit JSON come back as UTF-8 text; ZIP and XLSX come back as `base64`. Reads are paged by byte offset: the response includes `offset`, `bytes`, `has_more`, and `next_offset` when more data remains. Follow `next_offset` until `has_more` is false, then decode or concatenate the pages. The default page is 256 KB; each page is capped by the inline artifact limit and the response size limit.

#### Agent workflow: start, poll, read, download

1. Call `start_crawl`. It returns a job ID immediately and includes absolute artifact links for the job.
2. Poll `get_job` until `status` is terminal (`done`, `error`, or `cancelled`). Polling is the completion mechanism; there is no blocking wait call.
3. After `done`, use `get_results` for page rows, `get_audit` for audit sections, and `search_pages` for metadata or archive-backed text. HTML/text search requires `archive_html=true` or `audit=true`; audit retrieval requires `audit=true` on a spider job.
4. Use `get_artifact` for artifact metadata and an absolute URL, or download that URL directly with `GET`. If the client cannot reach the artifact host, use `read_artifact` to read the bytes over MCP and follow `next_offset` until complete.

#### Artifact URL contract

All artifact links are absolute and use this shape:

```text
<scheme>://<host><mcp-path>/artifacts/<job_id>/<kind>
```

With the default `/mcp` path, the URLs look like:

```text
https://crawler.example.com/mcp/artifacts/<job_id>/csv
https://crawler.example.com/mcp/artifacts/<job_id>/audit_strict_zip
https://crawler.example.com/mcp/artifacts/<job_id>/audit_json_inclusive
https://crawler.example.com/mcp/artifacts/<job_id>/audit_xlsx_strict
```

`<job_id>` is the ID returned by `start_crawl`. All seven artifact kinds are individually addressable:

| Kind | Content |
|------|---------|
| `csv` | Crawl report CSV |
| `audit_strict_zip` | Strict audit bundle (XLSX, CSVs, JSON, and manifest) |
| `audit_inclusive_zip` | Inclusive audit bundle (XLSX, CSVs, JSON, and manifest) |
| `audit_json_strict` | Strict audit JSON document |
| `audit_json_inclusive` | Inclusive audit JSON document |
| `audit_xlsx_strict` | Strict standalone audit workbook |
| `audit_xlsx_inclusive` | Inclusive standalone audit workbook |

#### Download behavior

- `GET` returns the file; `HEAD` returns headers only; range requests are supported.
- Responses set `Content-Type`, a sanitized `Content-Disposition` filename, and `Content-Length`.
- `404` for an unknown job, a UI-owned job, an unknown kind, or a missing or expired artifact.
- `409` for a job that exists but is not `done`.
- `405` for any method other than `GET` or `HEAD`, with an `Allow` header.
- `500` for an artifact generation failure, without filesystem paths or internal details.
- Downloads are restricted to MCP-created jobs. UI-created jobs remain browser-only and are not reachable through MCP artifact URLs.
- URLs expire with the existing job retention window. Once a job or its artifacts leave that window, the URL returns `404`.

#### Artifact base URL resolution

`GOFROG_MCP_ARTIFACT_BASE_URL` sets the canonical public origin (for example `https://crawler.example.com`) when configured. When it is empty, the server uses request-derived URLs: it reads `X-Forwarded-Proto` and `X-Forwarded-Host` from the incoming MCP request, then falls back to TLS and the `Host` header. The configured MCP path is appended after that base, so the base is an origin only and never the full artifact URL. See [DEPLOYMENT.md](DEPLOYMENT.md#mcp-artifact-downloads) for proxy requirements.

### Limits

The server enforces these defaults; per-job limits can lower them, never raise them. MCP limits do not change web UI or CLI behavior.

- **Max pages:** 5,000 pages per job
- **Max depth:** depth 10
- **Max list URLs:** 1,000 list URLs per list job
- **Max duration:** 30 minutes
- **Concurrency:** concurrency 20 per job
- **Max keywords:** 20 keywords of up to 100 characters each
- **Pattern length:** 512 characters for `search_pages` patterns
- **Max URL length:** 2,048 characters
- **Max request size:** 1 MB
- **Max response size:** 5 MB
- **Rate limit:** 60 tool calls per minute
- **Max concurrent jobs:** 4 global and 2 per token (the deployment uses one shared token)
- **Max retained jobs:** 100 finished jobs
- **Search limit:** 500 results per request
- **Inline artifacts:** 1 MB

Each limit has a `GOFROG_MCP_*` environment variable and a matching `--mcp-*` flag; see `.env.example`.

### Retention

Completed MCP jobs and their artifacts are kept for 24 hours after they finish. Running jobs are never reaped. With `GOFROG_MCP_PERSIST=true`, jobs are stored under `/results/jobs/<job_id>/`, so completed jobs survive a restart or redeploy for the retention window. A restart marks jobs that were running as `error` with code `interrupted`. Web UI jobs keep their existing 30-minute retention.

### Client configuration

Configure a remote Streamable HTTP MCP server with the MCP URL and the Bearer token issued by your proxy:

```json
{
  "mcpServers": {
    "go-frog": {
      "type": "http",
      "url": "https://crawler.example.com/mcp",
      "headers": {
        "Authorization": "Bearer <token>"
      }
    }
  }
}
```

Some clients infer Streamable HTTP from `url`; others need `"type": "http"` or a different top-level key. The server may return an `Mcp-Session-Id` header, and clients and proxies must forward it on subsequent requests.

---

## `--cli` flag (terminal wizard)

Run with `--cli` for the original interactive terminal wizard. Useful on servers, over SSH, or when you prefer typing to clicking:

```bash
./go-frog --cli
```

| Step | Prompt | What to enter |
|------|--------|----------------|
| 1 | **Choose Mode** | `1` = Spider, `2` = List |
| 2a | **Starting URL** | Full URL; `https://` assumed if omitted |
| 2b | **CSV path** | Full path to your input CSV (list mode) |
| 3 | **Keywords** | Pipe-separated, e.g. `pricing\|contact\|404`. Leave blank to skip |
| 4 | **Concurrency** | Positive integer; Enter alone uses default 10 |

Reports are saved to `results/YYYY-MM-DD-HH-MM-SS-<host>.csv`.

---

## Deploy to a server

See [DEPLOYMENT.md](DEPLOYMENT.md) for the full guide, including server-side rebuild and rollback steps.

Build locally, load on the server:

```bash
# On your local machine
docker compose build
docker save go-frog -o go-frog.tar

# Copy to server
scp go-frog.tar compose.yml user@your-server:~/

# On the server
docker load -i go-frog.tar
docker compose up -d
```

With oauth2-proxy in front, bind go-frog to localhost only and expose through the proxy:

```yaml
# compose.yml
services:
  go-frog:
    build: .
    ports:
      - "127.0.0.1:8080:8080"
    restart: unless-stopped
    dns:
      - 8.8.8.8
      - 1.1.1.1
```

---

## Crawling through a proxy

Set the proxy URL in the **Advanced** section of the web UI (not yet implemented — use environment variables for now), or set `HTTPS_PROXY` / `HTTP_PROXY` before launching:

```bash
HTTPS_PROXY=http://user:pass@proxy.example.com:3128 ./go-frog --cli
```

---

## How it works

### Spider mode

- Follows internal `http`/`https` links on the same site. `www.example.com` and `example.com` are treated as the same site for crawl boundaries. Subdomains like `blog.example.com` are not followed unless you start the crawl there.
- Redirects are **not auto-followed** — you get a row for the 301/302 URL itself, and the `Location` target is fetched separately.
- **Inlinks column** shows who linked to each page: `https://from/page>"anchor text" | https://other/>"other anchor"`.
- With the audit checkbox on, the crawl also writes the HTML archive and sitemap metadata described above. It reads `robots.txt`/sitemaps and fetches sitemap pages that link-following missed, so those pages appear as normal CSV rows. Sitemap fetch failures are warnings only and never fail the crawl. The `Inlinks` column is unchanged.

### List mode

- Visits each URL once. No link following, no inlink tracking.
- URLs are parsed from the textarea (server) or from every cell of a CSV file (CLI). Duplicates are removed.

### Keywords

- Each keyword is counted case-insensitively in the raw HTML of every 200 page.
- Non-200 rows get zero keyword counts and blank content columns.

---

## CSV columns

| Column | Meaning |
|--------|--------|
| **URL** | The requested URL |
| **StatusCode** | HTTP status (200, 301, 404, etc.) |
| **LoadTime** | Request duration |
| **ParentURL** | First inbound page (spider only) |
| **Inlinks** | All `fromURL>"anchor"` pairs (spider only) |
| **Title** | `<title>` (200 only) |
| **Description** | `<meta name="description">` (200 only) |
| **H1s** | All `<h1>` texts, joined with `|` (200 only) |
| **img alts** | All `img src>"alt"` pairs |
| **Robots** | `index/noindex, follow/nofollow` from headers and meta tags |
| **Search: …** | One column per keyword with occurrence count |

---

## Build from source

```bash
go build -trimpath -o go-frog .
go build -trimpath -o go-frog-audit ./cmd/audit
```

No C compiler or GUI dependencies required. The web UI is embedded. The Docker image includes both binaries.

---

## Limitations

- **JavaScript-rendered links** are not followed — only links in raw HTML.
- **Authentication / paywalls** are not supported.
- **Row order** in list mode may differ from input order due to concurrent fetching.
- Respect robots.txt, rate limits, and terms of service.

---

## License

Third-party libraries (see `go.mod`) have their own licenses.
