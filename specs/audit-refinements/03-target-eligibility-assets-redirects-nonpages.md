# 03 — Target Eligibility: Assets, Redirects, and Non-Pages

**Priority:** P1 (product behavior)
**Depends on:** 00
**Implement only this spec.** Do not change widget exclusions, sitemap discovery, or reporting columns here.

---

## 1. Goal

The internal linking audit should count **links to HTML pages only**.

- **Assets** (`.mp4`, `.pdf`, images, archives, etc.) are not internal links. Omit them entirely: no content count, no anchor analysis, no asset rows.
- **Redirect targets** are intentionally not credited. A link to a redirect URL does not count; if the site wants the link to count, it should link to the final URL.
- **404/unknown/non-page targets** are the main crawler's responsibility. The audit does not add a second link-health checker. It simply does not treat them as content page links.

This is a deliberate policy change from the comparison report:

- Report §7 said SGE's 5 `.mp4` links should be a separate asset category. The user's decision is simpler: omit them from the audit entirely.
- Report §6.2 recommended resolving redirects. The user's decision is the opposite: do not credit redirect aliases.
- Report §6.3 recommended broken-link/status columns. The user's decision is to leave 404 handling to the main crawler.

---

## 2. Relevant code

| File | Function / area | Current behavior |
|---|---|---|
| `internal/audit/url.go` | `AssetExtensions`, `IsAssetURL` | Defined but unused. |
| `internal/audit/audit.go` | link loop | Every internal HTTP link becomes a `Link`, including assets and redirect aliases. |
| `internal/audit/audit.go` | `Analyze` classification | All `DecisionContent` links go into `contentLinks`, even if the target is not a page. |
| `internal/audit/audit.go` | `redirectMap` | Built one hop at a time; only used to find the start page. |
| `internal/audit/audit.go` | inbound / PageRank / orphan | Inbound only credits targets in `Pages`, but content-link counts and anchors still include non-page targets. |
| `internal/audit/audit.go` | `Excluded` | No asset/redirect-target/non-page-target counters. |
| `spider.go` | redirect handling | Fetches redirect targets separately; main crawler owns redirect/404 behavior. |

---

## 3. Required behavior

### 3.1 Eligibility rule

A link is eligible for the internal linking graph only when:

1. It is an internal HTTP(S) link.
2. It is not in an excluded widget block (spec 01).
3. Its normalized target is present in `Result.Pages` as a 200 HTML page.
4. The target is not an asset extension.

If the target is not a page, the link is **not** a content page link. Do not fetch it, resolve it, or check its HTTP status in the audit.

### 3.2 Assets: omit entirely

- If the target (normalized, before/after redirect checks) matches `IsAssetURL`, skip the link before creating a `Link`.
- Do **not** add asset rows to `audit-links.csv`.
- Do **not** count assets in `contentLinks`, anchors, `ContentInbound`/`ContentOutbound`, PageRank, orphans, or any summary.
- Do **not** add an `AssetLinks` count. The user selected “no asset rows and no asset counts.”
- Keep `IsAssetURL`/`AssetExtensions` in `url.go` if useful as a helper; remove them only if nothing else uses them.
- If a URL has an asset extension but is genuinely a 200 HTML page in `Pages`, page wins. Check `Pages` before extension filtering.

Expected SGE result: 4 content page links, not 9. The 5 `.mp4` links disappear from the audit entirely.

### 3.3 Redirect targets: exclude from content

- A link whose normalized target is a redirect alias (manifest status 301/302/303/307/308 and not in `Pages`) is **not** a content page link.
- Do **not** resolve it to the final page for credit.
- Do **not** count it as content, nav, or boilerplate.
- Mark it `Decision = excluded`, `Reason = redirect_target` if it is retained for reporting (spec 04). It does not contribute to any graph metric.
- Keep `Redirects` reporting as-is for visibility, but do not add resolved/credited counts.

### 3.4 Non-page/404/unknown targets

- If the target is not in `Pages` and is not an asset or a known redirect alias, it is a non-page target.
- Do **not** add a target-status/broken-link detector. Do **not** inspect 404s.
- Do **not** count it as a content page link.
- Mark it `Decision = excluded`, `Reason = target_not_page` if retained for reporting. It does not contribute to any graph metric.
- The main crawler's normal results remain the place to inspect 404s.

### 3.5 Counters

Add to `Excluded`:

```go
RedirectTargetLinks int `json:"redirect_target_links"`
NonPageTargetLinks  int `json:"non_page_target_links"`
```

Do not add asset counters.

Add to `AuditSummary`:

```go
ContentPageLinks int `json:"content_page_links"`
```

Keep `ContentLinks` for backward compatibility. Define it as **content page link occurrences** after this spec; update `README.md` and the UI label. Do not add `ContentAssetLinks`.

### 3.6 PageRank and orphans

- No algorithm change is expected. `computePageRank` already uses only `Result.Pages` and filters edges to pages.
- With assets omitted and redirect targets excluded, the content graph is cleaner automatically.
- Add tests to prove assets/redirect aliases are not nodes and not counted.

### 3.7 Redirect diagnostics

`Result.Redirects` stays as a list of redirect responses with alias-level inbound counts. This is useful to see that a redirect exists, but it must not imply the final page received credit. Update the README wording:

> Redirect aliases are reported for visibility only. They do not pass link equity in this audit; link to the final URL if the link should count.

---

## 4. Tests

### 4.1 SGE-style media links

Page with:

- 4 links to HTML pages.
- 5 links to `.mp4`.
- 1 link to `.pdf`.

Assert:

- `len(result.Links) == 4`.
- No asset target appears in `result.Links`, `Anchors`, `ContentInbound`, or `ContentOutbound`.
- `audit-links.csv` has no asset rows.
- No asset counter exists in `Excluded`/`Summary`.
- PageRank node count equals the number of pages.

### 4.2 Redirect target is not credited

Archive with:

```text
/            200, links to /old
/old         301 -> /new
/new         200
```

Assert:

- The `/old` link is not a content page link.
- `/new.ContentInbound == 0` (no credit through `/old`).
- The link is `excluded`/`redirect_target` if it appears in the CSV.
- `Excluded.RedirectTargetLinks == 1`.

### 4.3 404/non-page target

Archive with a page linking to `/missing`, where `/missing` is 404 or absent.

Assert:

- The link is not a content page link.
- No `broken_links`/`fetch_failures`/target-status field is added.
- `Excluded.NonPageTargetLinks == 1` if retained for reporting.

### 4.4 HTML page with asset-like path

A URL ending in `.pdf` that is a 200 HTML page in `Pages` should count as a page. This guards the “page wins” rule.

### 4.5 Existing tests

`TestAnalyzeContentGraphAndOrphans` should pass; it has no assets or redirects. Update expected content counts only if the fixture changes.

---

## 5. Acceptance criteria

- [ ] Only links whose target is a 200 HTML page count as content.
- [ ] Asset links are omitted entirely: no rows, no counts, no graph metrics.
- [ ] Redirect-target links are excluded and do not credit the final page.
- [ ] Non-page/404 targets are not content and are not health-checked by the audit.
- [ ] SGE reports 4 content page links, not 9.
- [ ] No `fetch_failures`/`broken_links` duplication is added.
- [ ] `go test ./...` passes.
- [ ] No widget/sitemap/block-reporting changes.

---

## 6. Out of scope

- Class-based widget exclusions (spec 01).
- Sitemap discovery/orphans (spec 02).
- Full CSV inventory columns (spec 04).
- Block evidence (spec 05).
- Occurrence/unique metrics (spec 06).
- PageRank metadata (spec 07).
