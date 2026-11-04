# www.assuranceskin.com v2 — Fix Summary

**Date:** 2026-10-29
**Basis:** `misc/www.assuranceskin.com-v2-review.zip`, `specs/audit-refinements/`, and the policy decisions below.
**Scope:** deterministic auditor only. No changes to crawl behaviour, sitemap discovery, redirect policy, or 404 handling.

---

## 1. Locked policy decisions

| Decision | Outcome |
|---|---|
| Redirect handling | Keep current policy. Redirect aliases remain visibility-only and do not pass link equity. |
| Exclusion model | Class/id substring matching only, or selector-based overrides. No semantic `listing`/`related`/`broken` taxonomy and no new heuristic layers. |
| Crawl/archive behaviour | Keep what already works: save crawled pages, crawl and save sitemap URLs. No further core changes. |
| Content-container detection | Do not add `.theme-container` or similar as content roots. Fix false positives in the exclusion list instead. |
| 404 / broken links | Remains the main crawler's job. No target-status or broken-link columns in the audit. |
| Boilerplate | Remove the dead boilerplate machinery. Class matching is the committed mechanism. |
| v2 verification | The user will run the v2 audit themselves. |

---

## 2. Implemented fixes

### 2.1 Removed the false-positive `grid` exclusion

`grid` was matching layout wrappers, not just editorial widgets:

- `div.grid2` — homepage service directory
- `div.grid25` — product/blog card layout
- `div.grid-wrap` — content wrappers
- `li.col_grid` → `col-grid` — main-menu megamenu column

This caused the homepage to report zero content outbound and broke nav reachability for menu pages.

**Change:** removed `"grid"` from `DefaultExcludeSubstrings` and `broadExcludePatterns` in `internal/audit/classify.go`.

Other tokens were left intact. A scan of the audit data showed they matched only intended widgets (`recent-posts-body`, `blog-slider`).

### 2.2 Fixed invalid UTF-8 in `audit-links.csv`

`contextText()` truncated the `Context` field by bytes (`text[:limit-3]`), which split multi-byte characters and produced invalid UTF-8.

**Change:** `contextText()` now truncates by runes.

### 2.3 Removed dead boilerplate machinery

Removed:

- `DecisionBoilerplate`
- `boilerplateLinks` counter
- `Excluded.RepeatingBoilerplateLinks`
- `Structure.BlocksBoilerplate`
- `AuditSummary.BoilerplateLinks`
- associated decision/counting branches

Kept `Repeated` and `RepeatPageCount` as block metadata, per spec 05.

### 2.4 Added regression coverage

- `TestLayoutGridIsContent` — `content-block > grid2 > ul` must produce content links.
- `TestColGridMenuIsNav` — `li.col-grid` menus must be `nav`, not `excluded`.
- `TestLinkInventoryCSVIsUTF8` — generated `audit-links.csv` must be valid UTF-8.

---

## 3. Files changed

| File | Change |
|---|---|
| `internal/audit/classify.go` | Removed `grid` token; rune-safe `contextText`; removed `DecisionBoilerplate`. |
| `internal/audit/audit.go` | Removed boilerplate counters/fields and dead branches. |
| `internal/audit/exclusions_test.go` | Updated grouped-token test; added layout-grid and col-grid regressions. |
| `internal/audit/utf8_test.go` | New end-to-end UTF-8 CSV regression test. |
| `README.md` | Updated boilerplate wording and stale `grid` example. |

---

## 4. Validation

- `go test -count=1 ./...` passes.
- Live-homepage probe against the updated code:
  - the five `grid2` service blocks are now `content`,
  - `blk_a90eeb067030` is now `nav` instead of `excluded:col-grid`,
  - `blog-slider` blocks remain `excluded`.

A full v2 audit was not run. The user will run it.

---

## 5. Not changed / out of scope

- Redirect crediting.
- 404/broken-link reporting.
- Content-container heuristics.
- Sitemap/crawl core behaviour.
- Semantic exclusion taxonomy.
- Cross-page block aggregation. This remains a residual risk: `InsideContent` is AND-ed across pages, `PageHasContentContainer` is OR-ed, and `ExcludedClass` can come from a different instance than the features. Worth revisiting only if the next v2 run still shows inconsistent repeated-block decisions.

---

## 6. Expected effect on the next v2 run

- Homepage `Content Outbound` should no longer be 0.
- Menu pages such as `/medical-dermatology/hives/` should be nav-reachable.
- Content reachability, orphan counts, and PageRank should recover once the homepage has real content edges.
- `blog-slider` and `recent-posts-body` links should remain excluded.
- Product/blog layout grids (`grid25`, `grid-wrap`) will now be content unless specific class tokens are added to preserve listing exclusions.

---

## 7. Follow-ups

1. Re-run the v2 audit and compare against the review's numbers.
2. Decide whether any product/blog listing grids should still be excluded by specific class names (not the generic `grid` token).
3. Update `specs/audit-refinements/01-class-based-widget-exclusions.md`, which still lists `grid` in its example default list.
4. Treat the review's remaining evidence gaps as review-quality notes only:
   - v2 audit ZIP not present in the repo,
   - “same day = no crawl drift” not proven,
   - `+18 sitemap-only pages` not reconciled,
   - 29,320-row inventory does not reconcile with the reported categories.
