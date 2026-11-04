# www.assuranceskin.com v2 — Strict vs Inclusive Audit Rationale

**Date:** 2026-10-29
**Context:** Follow-up to `misc/www.assuranceskin.com-v2-fix-summary.md` and the after-fixes audit in `misc/www.assuranceskin.com-v2-after-fixes/`.
**Purpose:** Explain why a second, less strict audit profile was added, exactly what it counts, how it is produced, and what to check when comparing the next Strict and Inclusive results.

---

## 1. Why a second profile

The Strict audit deliberately excludes related posts, recent-post blocks, carousels, sliders, listing grids, and other widget-class matches. That is still the intended policy for the primary report: those links should not be treated as editorial content links by default.

However, the Strict policy can make pages look orphaned or unreachable when their only inbound links come from those widgets. That is useful as a strict warning, but it is not a complete picture of the site's internal link graph.

The Inclusive profile was added as a **second reporting view of the same crawl**, not as a replacement for Strict:

- **Strict** = the locked policy. Related/listing widgets do not pass link equity.
- **Inclusive** = a diagnostic view. Everything outside navigation, menus, and footers counts, including related posts and listing widgets.

Strict remains the default and the authoritative report for the locked policy.

---

## 2. What Inclusive means

Inclusive uses the same crawl archive as Strict. It does **not** re-crawl the site.

| Link type | Strict | Inclusive |
|---|---|---|
| Normal content links | Counted | Counted |
| Related posts / recent posts | Excluded by class | **Counted** |
| Blog/product listing grids | Excluded by class | **Counted** |
| Carousels, sliders, slideshows | Excluded by class | **Counted** |
| Navigation, menus, footer | Excluded | Excluded |
| Self-links | Excluded from content graph | Excluded from content graph |
| External links | Excluded | Excluded |
| Non-HTTP links | Excluded | Excluded |
| Redirect aliases | Excluded | Excluded |
| Non-page / 404 targets | Excluded | Excluded |
| Assets (images, PDFs, etc.) | Omitted | Omitted |

In short: **Inclusive counts every eligible internal HTML page link outside nav/menu/footer.** It does not count assets, external URLs, redirects, broken/non-page targets, or self-links as content edges.

This preserves the existing redirect and broken-link policies. It only relaxes the class-based widget exclusions.

---

## 3. How it is implemented

The audit already supported per-run filter overrides through `audit.Options.Filters` / `FilterOptions`. The new profile reuses that plumbing:

1. Run the normal Strict analysis with the user's configured nav/exclude filters.
2. Copy the Strict run's **effective navigation list**.
3. Run `Analyze` a second time with:
   - `NavSubstrings`: copied from Strict.
   - `ExcludeSubstrings`: empty.
   - `ForceContentSubstrings`: the distinct `excluded_class` tokens from the Strict run.
   - `Explicit: true`.
4. Write the Inclusive report to its own output directory.

Reusing the Strict nav list is important. With `Explicit: true`, sending an empty `NavSubstrings` list would also disable navigation detection and cause the Inclusive report to count menus and footers. The code copies the effective nav list instead.

Passing the Strict class tokens as `ForceContentSubstrings` is also important. It keeps widget blocks outside `<main>` from being reclassified as `nav` by the nav score after the class exclusion is removed. A block that is explicitly navigation still wins over the force-content list.

The Inclusive report recomputes the full result from the relaxed link graph:

- page content outbound/inbound
- unique link counts
- orphans and content-unreachable pages
- nav reachability
- PageRank and `top_share`
- block decisions
- anchor analysis
- CSV and JSON outputs

It is not a relabelled copy of the Strict links CSV, so its page-level metrics stay internally consistent with its own link set.

`pagerank_meta.excluded_widgets_excluded` now reflects the actual profile: `true` for Strict and `false` for Inclusive.

---

## 4. Output layout

Strict output remains in the run archive root, as before:

```text
results/<run>/audit.json
results/<run>/audit-pages.csv
results/<run>/audit-links.csv
results/<run>/audit-blocks.csv
```

Inclusive output is written alongside it:

```text
results/<run>/inclusive/audit.json
results/<run>/inclusive/audit-pages.csv
results/<run>/inclusive/audit-links.csv
results/<run>/inclusive/audit-blocks.csv
```

In the web UI, a completed audit job card now has **Strict** and **Inclusive** tabs. Each tab shows its own summary and download links:

```text
/api/crawl/<job>/audit/audit-links.csv
/api/crawl/<job>/audit/inclusive/audit-links.csv
```

The Strict tab is the default.

---

## 5. Baseline: Strict after the earlier fixes

The existing after-fixes audit in `misc/www.assuranceskin.com-v2-after-fixes/` is a Strict-only run:

| Metric | Value |
|---|---:|
| Pages | 177 |
| Content link occurrences | 719 |
| Unique content links | 665 |
| Orphans | 47 |
| Content unreachable from start | 115 |
| Nav links | 24,502 |
| Blocks | 221 content / 178 nav / 139 excluded |
| Top PageRank share | 9.35% |

This is the comparison baseline for the next Strict run. If the archive is the same and the code is unchanged, the Strict numbers should remain stable.

---

## 6. Approximate Inclusive effect

Reclassifying the existing Strict `excluded_class:*` rows as content gives an approximate Inclusive picture:

| Metric | Strict | Inclusive (approx.) |
|---|---:|---:|
| Unique content pairs | 665 | ~1,041 |
| Orphans | 47 | ~13 |
| Content reachable from start | 62 | ~82 |
| Pages with zero content outbound | 21 | ~10 |

These numbers are **approximate**. The real Inclusive run may differ because block-level classification is recomputed, not just link rows relabelled.

The bulk of the change is expected to come from:

- `recent-posts-body`: about 606 link rows
- `blog-slider`: about 406 link rows
- `slideshow-contain`: 2 link rows

---

## 7. What to check in the next run

### Strict report

- Strict should still be the default tab.
- Strict should keep the existing 719 content occurrences / 665 unique / 47 orphans / 115 unreachable if the same archive and filters are used.
- `recent-posts-body` and `blog-slider` links should still be `excluded_class:*` in Strict.
- Homepage content outbound should remain 57 occurrences / 55 unique.
- The menu block `blk_a90eeb067030` should remain `nav` / `nav_marker`.

### Inclusive report

- `filters_used.exclude_substrings` should be an explicit empty array.
- `filters_used.force_content_substrings` should list the Strict run's distinct `excluded_class` tokens.
- `filters_used.nav_substrings` should match the Strict run's effective nav list.
- `pagerank_meta.excluded_widgets_excluded` should be `false`.
- Related-post and blog-slider blocks should become `content`, not `excluded_class:*`.
- `audit-links.csv` should show those rows with `Decision=content` and `Reason=default_content` (or another content reason), not `excluded_class:*`.
- Inclusive content occurrences should be materially higher than Strict.
- Inclusive orphans should be lower than Strict.
- Nav/footer links should still be `nav`, not content.
- Self-links, external links, non-HTTP links, redirect targets, non-page targets, and assets should still be excluded or omitted.
- All three CSVs should be valid UTF-8.

### Important reachability note

Inclusive will **not** automatically make `/blog`, `/products`, or `/cn` content-reachable if those hubs are linked only from navigation. Their links from nav remain nav in both profiles. Inclusive may de-orphan many child pages through related-post and listing links, but the content-only reachability metric can still report nav-only hubs as unreachable from the start page.

That is expected, not a regression. A separate "content + nav union" view would be needed if full reachability through navigation is the goal.

---

## 8. Known limitations and deliberately unchanged behaviour

- No crawl, sitemap discovery, redirect policy, or 404 handling changes.
- No content-container heuristic changes.
- No semantic exclusion taxonomy.
- Redirect aliases still do not receive link equity.
- Broken/404 targets are still not reported with HTTP status columns.
- Self-links are still excluded from the content graph and PageRank.
- The audit now runs twice when enabled, so audit time roughly doubles. Crawl time is unchanged.
- If the user explicitly clears the Strict navigation list, Inclusive preserves that empty nav list. That is intentional: Inclusive follows the Strict run's effective nav policy.

---

## 9. Files touched

| File | Change |
|---|---|
| `server.go` | Runs the second audit with the inclusive profile; stores both results; profile-aware download routing; Strict/Inclusive job-card tabs. |
| `web/index.html` | Tab styling for the audit section. |
| `internal/audit/audit.go` | `excluded_widgets_excluded` metadata now reflects the active filter profile. |
| `internal/audit/inclusive_profile_test.go` | New test: related-post links excluded in Strict, counted in Inclusive, nav still excluded. |
| `server_test.go` | Tests for inclusive filter propagation, tab rendering, inclusive errors, and profile-specific file serving. |
| `README.md` | Documents Strict vs Inclusive semantics and outputs. |

Validation completed:

```text
go vet ./...
go test -count=1 ./...
go build ./...
```

All passed.

---

## 10. Handoff checklist for the next review session

When providing the new results, include both profiles from the **same crawl run**:

- Strict: `audit.json`, `audit-pages.csv`, `audit-links.csv`, `audit-blocks.csv`
- Inclusive: `inclusive/audit.json`, `inclusive/audit-pages.csv`, `inclusive/audit-links.csv`, `inclusive/audit-blocks.csv`

Also include the run directory name/timestamps and the `filters_used` block from both `audit.json` files. Keeping both profiles from one archive matters: the comparison is only valid if the crawl is identical.

---

## 11. Bottom line

The Inclusive tab is a **second lens on the same crawl**:

- Strict answers: “Which pages have real editorial/content links under the locked exclusion policy?”
- Inclusive answers: “Which pages are connected when related posts, listings, and carousels are allowed to count?”

Comparing the two should separate genuine content-graph gaps from pages that are only connected through widgets or navigation. Strict remains the policy report; Inclusive is the broader diagnostic view.
