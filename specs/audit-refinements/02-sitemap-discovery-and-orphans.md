# 02 — Sitemap Discovery and Orphan Reachability

**Priority:** P1 (correctness)
**Depends on:** 00
**Implement only this spec.** Do not change widget exclusions, assets, redirects, or block reporting here.

---

## 1. Goal

When **“Perform internal linking audit?”** is enabled, the crawl should also read the site's sitemap and fetch pages that normal link-following cannot reach. Those pages must appear in the audit and be flagged as **sitemap-only / content-unreachable**.

This fixes the real orphan concern:

- A page can be missing from the crawl entirely because no page links to it.
- A page can be “not orphaned” only because another orphan links to it.
- The sitemap gives the audit a complete page list to test reachability against.

The main crawler remains responsible for 404s and general site hygiene. The audit only uses the sitemap for coverage, not link-health duplications.

---

## 2. Relevant code

| File | Function / area | Current behavior |
|---|---|---|
| `spider.go` | `runSpider` | Starts at the start URL, follows internal `<a href>` links, optionally writes an archive. No sitemap support. |
| `spider.go` | `c.OnHTML("a[href]")` | Adds inbound and visits internal links. |
| `spider.go` | `c.OnError` | Enqueues redirect targets. |
| `scrape.go` | `attachPageRecording` | Records pages to the store/CSV/archive. |
| `internal/audit/archive.go` | `ArchiveWriter` / `ManifestEntry` | Records fetched URLs, status, redirects, HTML bodies. No discovery source. |
| `internal/audit/audit.go` | `Analyze` | Builds pages from manifest; no sitemap data. |
| `internal/audit/audit.go` | orphan block | `IsOrphan = ContentInbound == 0 && !IsStart`; no reachability. |
| `server.go` | audit toggle | Sets `settings.Audit` and `AuditArchiveDir`. |

---

## 3. Required behavior

### 3.1 Sitemap discovery (crawler, audit mode only)

When `settings.Audit` is true, before or during the crawl:

1. Fetch `https://<host>/robots.txt`.
2. Parse every `Sitemap:` directive (case-insensitive).
3. If none are found, try common paths in order:
   - `/sitemap.xml`
   - `/sitemap_index.xml`
   - `/sitemaps.xml`
4. Fetch each sitemap. Support:
   - `<urlset><url><loc>...</loc></url></urlset>` — page URLs.
   - `<sitemapindex><sitemap><loc>...</loc></sitemap></sitemapindex>` — recursively fetch child sitemaps (limit depth to 3; guard against cycles).
5. Keep only URLs on the same site (same `www`/apex policy as `hostMatchesSite`).
6. Normalize URLs using the audit package's `CanonicalURL`/`NormalizeURL` for deduplication, but keep the original URL for fetching.
7. Skip obvious non-HTML assets (`.mp4`, `.pdf`, images, archives) and non-HTTP schemes.
8. Deduplicate against URLs already discovered by link-following.

Sitemap failures must **not** fail the crawl. Record a warning in the archive/audit.

Askin's `robots.txt` points to `https://www.assuranceskin.com/sitemaps.xml`, so do not hard-code only `/sitemap.xml`.

### 3.2 Archive sitemap metadata

Write a `sitemap.json` file into the audit archive:

```json
{
  "fetched_at": "2026-10-29T...",
  "sitemaps": ["https://www.assuranceskin.com/sitemaps.xml"],
  "urls": ["https://www.assuranceskin.com/products/", "..."],
  "warnings": ["sitemap fetch failed: ..."]
}
```

Add:

```go
type SitemapMeta struct {
    FetchedAt time.Time `json:"fetched_at"`
    Sitemaps  []string  `json:"sitemaps,omitempty"`
    URLs      []string  `json:"urls,omitempty"`
    Warnings  []string  `json:"warnings,omitempty"`
}
```

Add to `Archive`:

```go
Sitemap *SitemapMeta
```

`LoadArchive` reads `sitemap.json` if present; absence is not an error (old archives and audit-off crawls have none).

### 3.3 Crawl sitemap-only pages

For each sitemap URL not already visited by link-following:

- Enqueue it through the same collector so it is fetched, recorded in the normal crawl results, and written to the archive.
- Respect the same site boundary and proxy/concurrency settings.
- Do not recurse into its links differently from a normal page; the collector handles that.
- If the URL redirects, the existing redirect handling applies; do not force-credit it.
- If it returns 404, the normal crawler records it; the audit does not duplicate 404 logic.

Mark discovery source in the archive so the audit can distinguish sitemap-only pages. Add to `ManifestEntry`:

```go
DiscoveredVia string `json:"discovered_via,omitempty"` // start, link, redirect, sitemap
```

Use `sitemap` for URLs enqueued only because they appeared in the sitemap. If a URL is also reached by a link, `link` is acceptable; the audit's reachability BFS is the final source of truth.

### 3.4 Audit fields

Add to `Page`:

```go
InSitemap   bool `json:"in_sitemap,omitempty"`
SitemapOnly bool `json:"sitemap_only,omitempty"`
```

Definitions:

- `InSitemap`: the page's normalized URL appears in `archive.Sitemap.URLs`.
- `SitemapOnly`: `InSitemap && !ContentReachableFromStart` — it was found via sitemap and cannot be reached from the start page through content links.

Add to `Result`:

```go
Sitemap *SitemapMeta `json:"sitemap,omitempty"`
```

Do not include the full sitemap URL list in `audit.json` if it is large; a count and warnings are enough. The archive `sitemap.json` holds the list.

### 3.5 Content reachability

Build a directed graph from content page links only:

- Nodes: `Result.Pages`.
- Edges: unique `source -> target` pairs where `Decision == content`, both are pages, and the target is eligible under spec 03.
- BFS from the resolved start page.
- Set `ContentReachableFromStart` on visited pages.
- Pages not visited are content-unreachable.

Add to `Page`:

```go
ContentOrphan              bool `json:"content_orphan"`
ContentReachableFromStart bool `json:"content_reachable_from_start"`
NavReachable               bool `json:"nav_reachable"`
```

Keep `IsOrphan` as a backward-compatible alias of `ContentOrphan`:

```go
ContentOrphan = Page.ContentInbound == 0 && !Page.IsStart
IsOrphan = ContentOrphan
```

`NavReachable` uses nav-only BFS from the start page (unique nav edges). Do not use `NavInbound > 0` as a proxy.

### 3.6 Orphan/sitemap reporting

Keep `Result.Orphans` as the direct content-orphan list for compatibility. Add:

```go
ContentUnreachable []string `json:"content_unreachable,omitempty"`
SitemapOnly        []string `json:"sitemap_only,omitempty"`
```

`OrphanType` values:

- `"content-orphaned, nav-reachable"`
- `"content-orphaned, not nav-reachable"`
- `"content-unreachable via orphan chain"`
- `"sitemap-only, content-unreachable"`

Rules:

- `Orphans` = direct content orphans (unchanged meaning).
- `ContentUnreachable` = all pages not reachable via content links from start, including direct orphans and orphan-chain pages.
- `SitemapOnly` = pages in the sitemap that are content-unreachable (subset of `ContentUnreachable`).
- A page in the sitemap but reachable by content links is **not** sitemap-only.

### 3.7 CSV and summary

Append to `audit-pages.csv`:

```text
In Sitemap
Sitemap Only
Content Orphan
Content Reachable From Start
Nav Reachable
```

Add to `AuditSummary`:

```go
ContentOrphans     int `json:"content_orphans"`
ContentUnreachable int `json:"content_unreachable"`
SitemapOnly        int `json:"sitemap_only"`
```

Keep `Orphans` for compatibility.

### 3.8 Behavior when audit is off

- Do not fetch robots.txt, sitemaps, or sitemap-only pages.
- Normal crawl results must be unchanged.
- The audit archive is not written, as today.

When audit is on, sitemap-only pages will also appear in the normal crawl CSV because they are fetched. Document this behavior. If this is undesirable, the implementing thread can fetch them into the archive only, but that is a larger change; default to normal crawl rows.

---

## 4. Tests

### 4.1 Sitemap parser

Unit-test:

- A `<urlset>` with three `<loc>` entries.
- A `<sitemapindex>` pointing to a child `<urlset>`.
- Duplicate URLs and external URLs.
- Malformed XML (warning, no crash).

### 4.2 Crawl includes sitemap-only page

Use `httptest` or the existing integration test pattern:

```text
/           200, links only to /a
/a          200
/sitemap.xml urlset: /a, /b
/b          200
```

Run with audit enabled. Assert:

- `/b` is fetched and appears in the archive.
- `Page.InSitemap == true`, `Page.SitemapOnly == true`, `ContentReachableFromStart == false`.
- `ContentUnreachable` contains `/b`.
- `Orphans` contains `/b` (direct inbound 0).

### 4.3 Auditor off

Run the same server with audit disabled. Assert `/b` is not fetched and no sitemap request occurs.

### 4.4 Sitemap failure

Make `/robots.txt` and `/sitemap.xml` return 500/404. Assert the crawl still completes normally and `SitemapMeta.Warnings` records the failure.

### 4.5 Orphan chain

```text
/                 start
/privacy-policy   no content inbound; content -> /blog
/blog             content inbound only from /privacy-policy
```

Assert `/privacy-policy` and `/blog` are both `ContentReachableFromStart == false`; `Orphans` contains only `/privacy-policy`; `ContentUnreachable` contains both.

### 4.6 Existing tests

`TestAnalyzeContentGraphAndOrphans` must still pass; add reachability assertions. `TestAnalyzeIsDeterministic` must pass.

---

## 5. Acceptance criteria

- [ ] With audit enabled, robots.txt/sitemap URLs are discovered and missing pages are crawled.
- [ ] Sitemap index files and urlset files are supported.
- [ ] Sitemap failures are non-fatal and reported.
- [ ] Sitemap-only pages are flagged and included in content-unreachable reporting.
- [ ] Content-reachability is BFS through content page links, not direct inbound.
- [ ] `Orphans` remains backward compatible.
- [ ] Audit-off crawls are unchanged.
- [ ] `go test ./...` passes.
- [ ] No widget exclusion, asset, redirect, or block reporting changes.

---

## 6. Out of scope

- 404/link-health detection (main crawler owns it).
- Redirect crediting (intentionally not done).
- Asset handling (spec 03).
- Class-based widget exclusions (spec 01).
- Reporting columns/block evidence (specs 04/05).
- Occurrence/unique metrics (spec 06).
- PageRank metadata (spec 07).
