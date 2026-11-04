# 07 — PageRank Methodology and Metadata

**Priority:** P3 (reporting quality)
**Depends on:** 03 (content page links only), 06 (occurrence vs unique)
**Implement only this spec.** Do not replace the PageRank algorithm unless a test exposes a real bug.

---

## 1. Goal

Make PageRank auditable by documenting and exporting:

- which nodes and edges are included,
- how self-links, dangling nodes, excluded widgets, redirects, and assets are handled,
- damping/tolerance/iteration settings,
- the primary ranking metric.

The comparison report's §8.4 correctly notes that old/new PageRank values are not directly comparable, but it incorrectly claims that media/redirect/404 targets are current PageRank nodes. They are not:

- `computePageRank` receives only `result.Pages`.
- Edges are built only when both endpoints are in `result.Pages`.
- Self-links are removed from `contentLinks`.
- After specs 01 and 03, excluded widgets, assets, and redirect targets never enter `contentLinks`.

This spec documents those facts and fixes a small top-share rounding issue.

---

## 2. Relevant code

| File | Function / area | Current behavior |
|---|---|---|
| `internal/audit/audit.go` | `computePageRank` | Nodes = `result.Pages`; unique source→target content edges; damping 0.85; tolerance 1e-8; max 100 iterations; dangling rank redistributed uniformly. |
| `internal/audit/audit.go` | PageRank edges | `edges[source][target] = struct{}{}`. |
| `internal/audit/audit.go` | `Page.PageRank` | Rounded to 6 decimals. |
| `internal/audit/audit.go` | `TopShare` | Sums **rounded** ranks before dividing. |
| `internal/audit/audit.go` | `PageRankRanking` | Sorted by rounded PageRank, then URL. |
| `README.md` | audit section | No PageRank methodology. |

---

## 3. Required behavior

### 3.1 Add `pagerank_meta`

```go
type PageRankMeta struct {
    NodeCount               int     `json:"node_count"`
    EdgeCount               int     `json:"edge_count"`
    Damping                 float64 `json:"damping"`
    Tolerance               float64 `json:"tolerance"`
    MaxIterations           int     `json:"max_iterations"`
    DanglingPolicy          string  `json:"dangling_policy"`
    Metric                  string  `json:"metric"`
    SelfLinksExcluded       bool    `json:"self_links_excluded"`
    ExcludedWidgetsExcluded bool    `json:"excluded_widgets_excluded"`
    AssetTargetsExcluded    bool    `json:"asset_targets_excluded"`
    RedirectTargetsExcluded bool    `json:"redirect_targets_excluded"`
}
```

Add to `Result`:

```go
PageRankMeta PageRankMeta `json:"pagerank_meta"`
```

Values:

- `NodeCount = len(result.Pages)`
- `EdgeCount = number of unique source→target page edges`
- `Damping = 0.85`, `Tolerance = 1e-8`, `MaxIterations = 100`
- `DanglingPolicy = "redistribute_uniformly"`
- `Metric = "unique_source_target_content_page_edges"`
- `SelfLinksExcluded = true`
- `ExcludedWidgetsExcluded = true` (spec 01)
- `AssetTargetsExcluded = true` (spec 03)
- `RedirectTargetsExcluded = true` (spec 03)

If a dependency is not implemented in a particular build, set its flag to `false` rather than claiming it. Add a test for the flags.

### 3.2 Compute TopShare from unrounded ranks

- Keep raw ranks from `computePageRank`.
- Compute `totalRank` from raw ranks.
- Compute `TopShare` from raw top rank / raw total.
- Round `Page.PageRank` to 6 decimals only for output.
- Sort the ranking by raw PageRank (then URL), not rounded values.

This may change `top_share` in the 6th decimal; document it.

### 3.3 Document methodology in README

Add a subsection:

- Nodes: 200 HTML pages in the archive; excludes assets, redirect aliases, non-page targets, and excluded widgets.
- Edges: one per distinct `source -> target` content page pair.
- Self-links: excluded before PageRank.
- Dangling nodes: rank redistributed uniformly each iteration.
- Damping/tolerance/iterations: 0.85, 1e-8, 100.
- Ranking metric: PageRank; page-table inbound/outbound counts are occurrences unless labeled unique.
- `top_share` = raw top PageRank / raw sum.

### 3.4 Optional

Do not add a new CSV unless requested. `audit-pages.csv` + `pagerank_ranking` are sufficient.

---

## 4. Tests

### 4.1 Metadata

Run `Analyze` on a small archive and assert:

- `PageRankMeta.NodeCount == len(result.Pages)`.
- `EdgeCount` equals unique content page pairs.
- Damping/tolerance/iterations/policy/metric values.
- Exclusion flags are true when specs 01/03 are in place.

### 4.2 Sum and top share

For a graph with no dangling nodes, assert raw PageRank sums to ~1.0 (within 1e-6). Assert `TopShare` equals raw top / raw sum, not the rounded sum.

Add a package-level test for `computePageRank` if it is not already covered.

### 4.3 Only pages are nodes

Build an archive with an asset link, a redirect target, and an excluded related-post block. Assert:

- `NodeCount` equals the number of HTML pages only.
- None of those targets appears in `PageRankRanking`.

### 4.4 Existing tests

`TestAnalyzeIsDeterministic` must pass. Update expected `top_share` only if a test asserts it.

---

## 5. Acceptance criteria

- [ ] `audit.json` includes `pagerank_meta`.
- [ ] `top_share` is computed from unrounded ranks.
- [ ] Ranking/output remain deterministic.
- [ ] README documents the methodology.
- [ ] PageRank nodes are HTML pages only; excluded widgets/assets/redirect targets are not nodes.
- [ ] `go test ./...` passes.
- [ ] No algorithm replacement or weighting change.

---

## 6. Out of scope

- Personalized/topic-sensitive PageRank.
- Weighting by nav links or anchor text.
- Sitemap/reachability (spec 02).
- Occurrence/unique page metrics (spec 06).
- Changing the content graph beyond specs 01/03.
