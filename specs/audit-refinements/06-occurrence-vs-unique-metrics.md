# 06 — Occurrence vs Unique Metrics

**Priority:** P2 (reporting quality)
**Depends on:** 03 (content page links only), 04 (stable inventory)
**Implement only this spec.** Do not change classification, sitemap, or PageRank here.

---

## 1. Goal

Report both link **occurrences** and **unique source→target pairs**, and make the primary ranking metric explicit.

Current behavior:

- `Page.ContentInbound`/`ContentOutbound` count occurrences.
- PageRank already deduplicates edges to one per `source -> target`.
- Anchor over-optimization already counts distinct sources.
- `Summary.ContentLinks` is an occurrence count, but the name does not say so.

After spec 03, these metrics cover **content page links only** (no assets, no redirects, no excluded widgets). Numbers will differ from the old sample, but the principle is unchanged: a card with an image link and a title link is two occurrences and one unique pair.

---

## 2. Relevant code

| File | Function / area | Current behavior |
|---|---|---|
| `internal/audit/audit.go` | `Page` | `ContentOutbound`, `ContentInbound`, `NavInbound`. |
| `internal/audit/audit.go` | inbound block | Loops over `contentLinks` and increments per occurrence. |
| `internal/audit/audit.go` | PageRank edges | `edges[source][target] = struct{}{}` — unique pairs. |
| `internal/audit/audit.go` | `RankEntry` | `URL`, `PageRank`, `ContentInbound` (occurrences). |
| `internal/audit/audit.go` | `AuditSummary` | `ContentLinks` (occurrences). |
| `internal/audit/audit.go` | `writePagesCSV` | Occurrence counts only. |

---

## 3. Required behavior

### 3.1 Page metrics

Add to `Page`:

```go
ContentInboundOccurrences  int `json:"content_inbound_occurrences"`
ContentInboundUnique       int `json:"content_inbound_unique"`
ContentOutboundOccurrences int `json:"content_outbound_occurrences"`
ContentOutboundUnique      int `json:"content_outbound_unique"`
```

Compatibility:

- Keep `ContentInbound`/`ContentOutbound` as aliases of the occurrence counts.
- Do not change their meaning; add the explicit fields alongside.

Definitions:

- **Inbound occurrences:** content page link occurrences targeting the page.
- **Inbound unique:** distinct source pages linking to the target through content page links.
- **Outbound occurrences:** content page link occurrences on the page.
- **Outbound unique:** distinct target pages linked from the page.

### 3.2 Result and summary metrics

Add to `Result`:

```go
ContentLinkOccurrences int `json:"content_link_occurrences"`
UniqueContentLinks     int `json:"unique_content_links"`
```

- `ContentLinkOccurrences == len(result.Links)`.
- `UniqueContentLinks` = distinct `(Source, Target)` pairs.

Add to `AuditSummary`:

```go
ContentLinkOccurrences int `json:"content_link_occurrences"`
UniqueContentLinks     int `json:"unique_content_links"`
```

- Keep `ContentLinks` for compatibility; define it as `ContentLinkOccurrences`.
- Do not rename it in this spec; note a v2 rename for final integration if desired.

### 3.3 Rank entries

Add to `RankEntry`:

```go
ContentInboundUnique int `json:"content_inbound_unique"`
```

- Keep `ContentInbound` as occurrences.
- PageRank remains based on unique source→target edges.
- README: **PageRank uses unique source→target edges; page-table inbound/outbound columns are occurrences unless labeled unique.**

### 3.4 CSV and UI

Append to `audit-pages.csv`:

```text
Content Inbound Unique
Content Outbound Unique
```

Keep existing occurrence columns. Optionally rename them to `... Occurrences`; if renamed, update README and consumers.

`server.go` job card uses `AuditSummary.ContentLinks` as “content links.” Update the label to “content link occurrences” or show both counts.

---

## 4. Tests

### 4.1 Duplicate occurrences

```text
/          content -> /a (image link)
/          content -> /a (title link)
/          content -> /b
/a         content -> /
```

Assert:

- `/a`: `ContentInbound == 2`, `ContentInboundUnique == 1`.
- `/`: `ContentOutbound == 3`, `ContentOutboundUnique == 2`.
- `Result.ContentLinkOccurrences == 4`, `UniqueContentLinks == 3`.
- PageRank edge count is 3, not 4.

### 4.2 Excluded links do not count

A related-post block with two links must not appear in occurrence or unique counts (spec 01).

### 4.3 Summary

Assert `Summary.ContentLinkOccurrences == len(result.Links)` and `Summary.UniqueContentLinks == countUniquePairs(result.Links)`.

### 4.4 Existing tests

`TestAnalyzeContentGraphAndOrphans` asserts `ContentInbound` values; those remain occurrence counts. Update expected content links because related-posts are now excluded (spec 01).

---

## 5. Acceptance criteria

- [ ] Page occurrence and unique metrics are both exported.
- [ ] Result/summary distinguish occurrences and unique pairs.
- [ ] PageRank still uses unique source→target edges.
- [ ] CSV and README make the metric names explicit.
- [ ] `go test ./...` passes.
- [ ] No classification, sitemap, or target-eligibility changes.

---

## 6. Out of scope

- PageRank metadata (spec 07).
- Sitemap/reachability (spec 02).
- Block evidence (spec 05).
- Excluded widget rules (spec 01).
