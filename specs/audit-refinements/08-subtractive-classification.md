# 08 — Purely Subtractive Block Classification

**Priority:** P0 (classification policy)
**Depends on:** 01 (class-based exclusions), 01a (filter editor)
**Supersedes:** the content-root decision path in 00, and the `nav_score` / `inside_content` / `page_has_content_container` evidence fields in 05.
**Implement only this spec.** Do not change the nav substring list, class-exclusion matching, target eligibility, redirect policy, crawl behavior, or metrics here.

---

## 1. Goal

Make block classification **purely subtractive**:

```text
start with content
remove explicit overrides
remove class-excluded widgets (Strict)
remove navigation/header/footer
everything left is content
```

The auditor must stop trying to **find** a content container. There is no `inside main`, no `page has content container`, and no nav-score fallback in the decision.

This removes the failure described in the v2 review §6.7 and reproduced in v4:

- `page_has_content_container = true` for only 14 of 538 blocks.
- `inside_content = false` for all 538 blocks.
- Four blocks on the Askin site are still `nav` because `high_nav_score_no_main`, even though they are not navigation:
  - `blk_b77d8bd9d76c` (`div.container`, homepage blog section)
  - `blk_4c29d398937c` (`div.container`, `/cn` blog section)
  - `blk_773177ee626d` (`div.theme-container`, homepage)
  - `blk_8c9f8025114e` (`div.theme-container`, `/cn`)

The user's policy decision is explicit: **do not look for content roots.** The theme does not use `<main>`; its content is in `<section>` and `<div class="container">`. Trying to infer a content root from those wrappers is not reliable and must not decide link status.

---

## 2. Current behavior

| File | Function / area | Current behavior |
|---|---|---|
| `internal/audit/classify.go` | `classifyBlock` | Overrides → excluded class → nav marker → force-content class → `InsideContent` → `PageHasContentContainer && NavScore >= 0.4` → `!PageHasContentContainer && NavScore >= 0.7` → default content. |
| `internal/audit/classify.go` | `blockFeaturesForNode` | Computes `InsideContent` via `hasContentContainerAncestor`, `PageHasContentContainer` from the page, and `NavScore` from position/link density. |
| `internal/audit/classify.go` | `hasContentContainerAncestor` | Treats `main`, `article`, and `[role="main"]` as content roots. |
| `internal/audit/classify.go` | `pageHasContentContainer` | Same tags, page-level. |
| `internal/audit/classify.go` | `isContentRootNode` | Used both as an exclusion-walk boundary and as a content-root test. |
| `internal/audit/classify.go` | `Block` / `blockFeatures` | Exports `nav_score`, `inside_content`, `page_has_content_container`. |
| `internal/audit/audit.go` | `writeBlocksCSV` | Exports `Nav Score` and `Inside Content` columns. |
| `internal/audit/filters.go` | `FilterOptions` | Carries `force_content_substrings`, added as a workaround for the nav-score fallback. |
| `server.go` | `inclusiveAuditFilters` | Builds `force_content_substrings` from the Strict run's excluded class tokens. |

The force-content list was a workaround, not a policy. Once classification is subtractive it is unnecessary: in Inclusive, a widget that Strict excluded by class is simply not excluded, so it falls through to content.

---

## 3. Required behavior

### 3.1 New decision order

`classifyBlock` must become exactly this order:

```text
1. force_exclude override                         → nav / override_force_exclude
2. force_content override                         → content / override_force_content
3. excluded class (block or ancestor)             → excluded / excluded_class:<match>
4. nav marker                                     → nav / nav_marker
5. otherwise                                      → content / default_content
```

Remove these branches entirely:

- `InsideContent` → `inside_main`
- `PageHasContentContainer && NavScore >= 0.4` → `outside_main_high_nav_score`
- `!PageHasContentContainer && NavScore >= 0.7` → `high_nav_score_no_main`

No link-dense block can become `nav` merely because it is outside `<main>` or because its link-text ratio is high. Only an explicit nav marker (nav/header/footer/menu patterns) can.

Keep the existing force overrides from `Overrides` (`force_content` / `force_exclude`). They are deliberate per-site selectors, not content-root inference.

### 3.2 Remove content-root detection from decisions

- Delete `hasContentContainerAncestor` and `pageHasContentContainer` from the decision path.
- Remove `InsideContent` and `PageHasContentContainer` from `blockFeatures`, `Block`, JSON, and CSV.
- Remove `NavScore` from `blockFeatures`, `Block`, JSON, and CSV.
- Remove the `inside_main`, `outside_main_high_nav_score`, and `high_nav_score_no_main` reasons and their constants.
- `blockFeaturesForNode` no longer needs `pageHasContent` or `forceContentPatterns` parameters.

Keep `NavMarker`, `ExcludedClass`, `LinkCount`, `LinkTextRatio`, `DirectAnchorCount`, and `Position` for evidence, but none of them may change the decision except `ExcludedClass` and `NavMarker`.

### 3.3 Exclusion-walk boundary

`excludedClassForBlock` and `excludedClassForAnchor` currently stop their ancestor walk at `isContentRootNode`. That helper must no longer be a content signal.

- Rename it to a private exclusion-boundary helper, e.g. `isExclusionWalkBoundary`.
- Use it only to bound ancestor class matching in `excludedClassForBlock` / `excludedClassForAnchor`.
- Do not export it, and do not use it to set any decision or evidence field.
- Do not change class matching tokens, broad-pattern limits, or depth rules in this spec.

This keeps widget exclusion behavior stable while removing the content-root concept from classification.

### 3.4 Remove the `force_content_substrings` workaround

- Remove `FilterOptions.ForceContentSubstrings`.
- Remove `ForceContentSubstrings` from `FiltersUsed`.
- Remove `strictExcludedClassTokens` from `server.go`.
- `inclusiveAuditFilters` should again return only:
  - the Strict run's effective `NavSubstrings`,
  - `ExcludeSubstrings: nil`,
  - `Explicit: true`.
- `resolveFilterPatterns` returns to `(nav, exclude, explicit)`.
- Keep the `exclude_substrings: []` serialization fix. Inclusive must still emit an explicit empty array.

Inclusive and Strict now differ only by the exclusion list. That is the intended model.

### 3.5 Reporting schema

Remove these from `audit.json` blocks and `audit-blocks.csv`:

```text
nav_score
inside_content
page_has_content_container
Nav Score column
Inside Content column
```

Keep:

```text
nav_marker
excluded_class
position
link_count
link_text_ratio
direct_anchor_count
decision
reason
```

This is a deliberate breaking schema change. It supersedes the corresponding fields in spec 05. The v2/v3/v4 reports keep their historical columns; new runs use the reduced schema.

### 3.6 Link-level behavior

No change to the link decision ladder beyond the block decisions it inherits:

```text
force_exclude → force_content → excluded_class → self → nav_marker/nav_excluded
→ block decision → anchor_content
```

A link inside a block that is now `content` becomes content unless it has its own exclusion, self, or nav marker.

---

## 4. Tests

Update or replace:

| Test | Change |
|---|---|
| `TestClassifyBlockHybridRules` | Remove `inside main`, `outside main high score`, and `no main strong score` cases. Add: high nav score outside any content root is `content/default_content`; nav marker still `nav/nav_marker`; excluded class still `excluded/excluded_class:*`. |
| `TestInclusiveForceContentOverridesNavHeuristic` | Replace with a test that an outside-main, link-dense widget is `content` in both Strict (unless class-excluded) and Inclusive. |
| `TestInclusiveForceContentDoesNotOverrideNavMarker` | Keep the nav-marker precedence assertion, but remove the force-content filter from the fixture. |
| `TestForceContentSubstringsInFiltersUsed` | Delete. |
| `TestInclusiveAuditFiltersKeepNav` | Remove force-content expectations; assert only nav reuse, empty excludes, and `Explicit`. |
| `TestEmptyExcludeSubstringsSerializesAsArray` | Keep. |
| Block CSV/JSON tests | Remove assertions for `nav_score`, `inside_content`, `page_has_content_container`. |
| New regression | A page with no `<main>` and a link-dense `<div class="theme-container">` must classify the block as `content/default_content`, not `nav`. |
| New regression | A link-dense block under a `nav`/`header`/`footer` marker must remain `nav/nav_marker`. |
| New regression | A Strict `excluded_class` widget becomes `content/default_content` in Inclusive without `force_content_substrings`. |

---

## 5. Acceptance criteria

1. `classifyBlock` contains only the five steps in §3.1.
2. `grep` finds no decision use of:
   - `high_nav_score_no_main`
   - `outside_main_high_nav_score`
   - `inside_main`
   - `InsideContent`
   - `PageHasContentContainer`
   - `NavScore`
   - `ForceContentSubstrings`
3. `audit-blocks.csv` no longer has `Nav Score` or `Inside Content`.
4. `audit.json` blocks no longer have `nav_score`, `inside_content`, or `page_has_content_container`.
5. Inclusive `filters_used` contains `exclude_substrings: []` and no `force_content_substrings`.
6. The four v4 `high_nav_score_no_main` blocks become `content/default_content`.
7. Expected v4 delta when re-run on the same archive:

| Metric | v4 Strict | After | v4 Inclusive | After |
|---|---:|---:|---:|---:|
| Content occurrences | 719 | **724** | 1,731 | **1,736** |
| Unique content pairs | 665 | **670** | 1,039 | **1,044** |
| Orphans | 47 | **47** | 13 | **13** |
| Content unreachable | 115 | **113** | 95 | **92** |

8. `go test -count=1 ./...`, `go vet ./...`, and `go build ./...` pass.

---

## 6. Out of scope

- Changing nav/footer substring defaults.
- Changing exclusion substring defaults or broad-token depth rules.
- Changing target eligibility, redirects, non-page targets, assets, or 404 behavior.
- Changing sitemap discovery, PageRank math, or occurrence/unique metric definitions.
- Adding new content-root heuristics. This spec removes them; it does not replace them with a better detector.
- Reclassifying links by text density, semantic similarity, or repeated signatures.

---

## 7. Files likely touched

| File | Change |
|---|---|
| `internal/audit/classify.go` | Subtract-only `classifyBlock`; remove content-root/nav-score features and reasons; rename exclusion boundary helper. |
| `internal/audit/audit.go` | Remove `NavScore`, `InsideContent`, `PageHasContentContainer` from `Block`; update `writeBlocksCSV`; stop passing removed feature inputs. |
| `internal/audit/filters.go` | Remove `ForceContentSubstrings`; restore `(nav, exclude, explicit)`. |
| `server.go` | Remove `strictExcludedClassTokens`; simplify `inclusiveAuditFilters`. |
| `internal/audit/classify_test.go` | Replace hybrid classification tests with subtractive cases. |
| `internal/audit/inclusive_profile_test.go` | Update inclusive tests; remove force-content workaround tests. |
| `internal/audit/filters_test.go` | Remove force-content serialization test; keep empty-exclude test. |
| `internal/audit/block_evidence_test.go` | Remove removed evidence-field assertions. |
| `server_test.go` | Update inclusive filter test. |
| `README.md` | Document subtractive classification and the reduced block schema. |

---

## 8. Notes

- The v2 fix summary said “no content-container heuristic changes.” This spec explicitly reverses that for classification: the heuristic is removed, not improved.
- The Inclusive profile is now conceptually simple: run the same crawl, reuse Strict's nav list, clear the widget exclusions, and let the subtractive rule decide.
- Do not preserve `inside_content` or `page_has_content_container` as “diagnostics.” They were the broken detector, and keeping them invites the same false evidence. Remove them from new reports.
