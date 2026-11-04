# Audit Refinements — Overview and Decisions

**Status:** planning only. No production code has been changed.

**Purpose:** turn the `misc/audit-comparison-bundle` comparison into a small, dependency-ordered set of implementation specs. Send **one spec per thread**, in the order below, and tell that thread to implement only the named spec.

**Source report:** `audit-comparison-bundle/deterministic-audit-comparison.md`
(the extracted bundle used for this review is under `/tmp/opencode/acb/audit-comparison-bundle/`; the original is `misc/audit-comparison-bundle.zip`).

**Codebase reviewed:** `internal/audit/` (`audit.go`, `classify.go`, `url.go`, `archive.go`), `spider.go`, `scrape.go`, `cmd/audit/main.go`, `server.go`, and the existing tests.

---

## 1. Product decisions

These decisions override the comparison report where they conflict:

1. **Related posts, doctor sliders, and carousels are not internal links.**
   They must be excluded by class matching. This is intentional, not a false negative.
   - Use a new `excluded` decision with reason `excluded_class:<pattern>`.
   - Do not recover these links as content.
   - Do not infer exclusion from content similarity or repeated signatures.
   - Use partial class matching for: editorial feeds (`related`, `recommend`, `recirc`, `similar`, `suggest`, `trending`, `popular`, `latest`, `recent`, `curated`, `spotlight`, `picks`, `stream`, `grid`, `teaser`, `crosslink`, `cross-promo`, `read-next`) and motion/carousels (`slider`, `slide`, `carousel`, `swiper`, `slick`, `splide`, `glide`, `flick`, `marquee`, `ticker`, `rotator`, `strip`, `lightbox`, `filmstrip`, `scroller`).
   - Nav/footer partials (`navbar`, `nav`, `menu`, `megamenu`, `meganav`, `subnav`, `drawer`, `offcanvas`, `flyout`, `masthead`, `toolbar`, `appbar`, `tabbar`, `footer`, `subfooter`, `prefooter`, `colophon`, `socket`, `legal`, `imprint`, `impressum`, `copyright`, `bottombar`, `to-top`, `back-to-top`, `sidebar`, `breadcrumb`) remain nav detection. Do **not** apply `header` as a raw substring; it matches content headings like `column-header`.
   - Match generic `slider`/`slide`/`swiper`/`slick`/`strip`/`stream`/`grid` on the block itself, not distant ancestors, so built-in controls don't wipe out surrounding content.
   - The broad tokens (`grid`, `stream`, `strip`, `slide`, `latest`, `popular`, `picks`) are configurable so a site can remove a false positive.
   - The lists are editable on the run page when the audit is enabled. All defaults are pre-filled/enabled; removing a line disables it for that run (spec 01a).

2. **Redirects do not pass link equity.**
   A link to a redirect URL does not count, and the final page gets no credit. If the site wants the link to count, it should link to the final URL.
   - Do not resolve redirect aliases for graph metrics.
   - Redirects may remain visible in `Redirects` for diagnostics only.

3. **Assets are not part of the internal linking audit.**
   `.mp4`, `.pdf`, images, archives, etc. are omitted entirely: no rows, no counts, no anchor analysis.
   - Only links whose target is a 200 HTML page count.

4. **404s belong to the main crawler.**
   The audit must not add a second link-health/404 checker, broken-link column, or fetch-failure list.

5. **Sitemap is used for coverage when the audit is enabled.**
   Fetch robots.txt/sitemap URLs, crawl pages that link-following cannot reach, include them, and flag them as sitemap-only/content-unreachable.

6. **Repeated-signature boilerplate is replaced by class matching for these widgets.**
   Do not delete a block merely because its target signature appears on 2+ pages. Exclude the known widget classes explicitly.

---

## 2. How to use this folder

1. Start with this file in a new thread.
2. Send the numbered specs one at a time, in order.
3. Each spec is scoped to one change and lists its dependencies.
4. Each spec ends with acceptance criteria and tests. Stop when those are met, run the tests, and report changed files.
5. If a spec conflicts with this overview, this overview wins on product decisions; the spec wins on implementation detail.

### Global constraints

- The auditor remains **offline and deterministic**: `Analyze` reads a saved archive and never fetches the network.
- Sitemap fetching is the **only** new network behavior, and only in the crawler when the audit is enabled.
- Do not change the crawler's existing CSV columns or normal crawl behavior when the audit is off.
- Keep `NormalizeURL`'s graph identity (https, apex host, no trailing slash, tracking params removed).
- Prefer additive JSON fields. Do not bump `AuditVersion` in individual specs; coordinate one bump at final integration if needed.
- Update `README.md` and `server.go` links/labels when an exported artifact or metric changes.
- Tests must be deterministic and must not depend on a live site.

### Verification

```bash
go test ./...
go build ./...
```

If Go is unavailable:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go test ./...
```

The bundle contains outputs, not crawl archives. Use synthetic archives with `writeTestArchive` in `internal/audit/audit_test.go`, plus `httptest` for sitemap/crawl tests.

---

## 3. Shared vocabulary

| Term | Meaning |
|---|---|
| **Graph identity** | Result of `NormalizeURL`: `https://<apex-host><path>`, no trailing slash, no `www.`, no fragment, no tracking params. Used as `Page.URL` and `Link.Target`. |
| **Final URL** | Actual 200 response URL (`Page.FinalURL`). May be `www` while the graph identity is apex. |
| **Content page link** | A link whose block decision is `content` and whose target is a 200 HTML page in `Result.Pages`. |
| **Excluded link** | A link reported with `Decision=excluded` because it is in a class-matched widget, targets a redirect, or targets a non-page. Never counted in the content graph. |
| **Asset** | Non-page file (`.mp4`, `.pdf`, image, archive). Omitted entirely. |
| **Redirect target** | A link target that is a 301/302/303/307/308 URL and not a page. Excluded from content; no final-page credit. |
| **Non-page target** | An internal target not in `Pages` and not an asset/redirect. Excluded from content; no health check. |
| **Occurrence** | One `<a href>` element. |
| **Unique link** | One distinct `source -> target` pair. |
| **Content orphan** | Non-start page with zero content inbound occurrences. |
| **Content-unreachable** | Not reachable from start through content page links only. |
| **Sitemap-only** | In the sitemap but not content-reachable from start. |
| **Nav-reachable** | Reachable from start through nav links only (BFS, not `NavInbound > 0`). |
| **Block** | Link-dense DOM subtree selected by `nearestBlock`, aggregated by target signature. |

---

## 4. Fact check on the comparison report

| Report claim | Verdict | What to do |
|---|---|---|
| §4.1/§4.2 related-post and home/CN carousel blocks are “false negatives.” | **Not a bug — intended exclusion** | These widgets must not count. Spec 01 makes the exclusion explicit and consistent. The current inconsistency (89 content vs 3 nav vs 1 boilerplate) is the real issue. |
| §5.1 “content-container detection is failing” and §5.2 same class gets different decisions. | **Valid inconsistency, wrong fix** | Do not add `.theme-container` as a content root or recover these blocks as content. Fix by class-based exclusion (spec 01). Spec 08 subsequently removes content-root detection from classification entirely. |
| §5.1 recommendation to use `.theme-container` as a content root. | **Reject** | It wraps the whole page including header/nav. It must not make nav content. |
| §6.1 “canonical host is inconsistent” / use `final_url` as page key. | **Reject the fix** | `NormalizeURL` intentionally merges `www`/apex, http/https, trailing slash. Keep it. `FinalURL` already exposes the real URL; no graph-key change. |
| §6.2 redirect targets are not resolved. | **Intentional behavior** | Do not resolve or credit. Spec 03 excludes redirect-target links from content. |
| §6.3/§7 assets and broken targets. | **Assets: valid; 404s: out of scope** | Assets are omitted entirely (spec 03). 404s stay with the main crawler. No broken-link/status columns. |
| §8.2 orphan logic is direct-inbound only. | **Valid** | Add content/nav reachability and sitemap-only reporting (spec 02). |
| §8.4 “new tool includes non-page targets as PageRank nodes.” | **Incorrect** | `computePageRank` already uses only `result.Pages`. Assets/redirect aliases are not nodes. Spec 07 documents/verifies. |
| §4.3 repeated blocks can over-delete. | **Valid, but fix is class matching** | Remove the repeated-signature boilerplate rule for these widgets; exclude by class (spec 01). |
| §9 anchor extraction: export raw + normalized anchors. | **Valid, small** | Spec 04. |
| §10.1/§10.2 no full inventory / no excluded reasons. | **Partly stale** | `audit-links.csv` already has every internal link with decision/reason. Missing: external/non-HTTP rows, raw anchors, explicit target types (spec 04). |
| §10.3 blocks lack signature/anchors/snippet. | **Valid** | Spec 05. |
| §10.4 `auto_label`, `auto_excluded`, `effective_label` are null. | **Not applicable** | Those fields do not exist. Do not add them. |
| §10.5 `content_hint`/`inside_content` are dead. | **Partly valid** | `content_hint` becomes dead when the related-hint path is removed; remove or deprecate it (spec 05). `inside_content` is removed from classification and reporting by spec 08. |
| §10.6 no crawl failure list. | **Out of scope** | 404/failure handling is the main crawler's job. Do not duplicate. |
| §10.7 external/non-HTTP counts differ with no list. | **Valid** | Spec 04 adds rows for those categories. |

### No-action items

- Self-links excluded: correct.
- Site drift (`/about-us` → `/`): correct, not a tool miss.
- Old `/cn/medical-dermatology/eczema` redirect: intentional, no page.
- Trailing-slash/`www`/http collapse: intentional.
- Image alt preservation and whitespace normalization: preserve.
- Deeply nested nav exclusion: preserve.
- 20-item over-optimization cap: preserve.

---

## 5. Spec index and order

| # | File | Priority | Depends on | Outcome |
|---|---|---|---|---|
| 00 | `00-overview-and-fact-check.md` | — | — | Decisions, corrections, order |
| 01 | `01-class-based-widget-exclusions.md` | P0 | 00 | Exclude related posts, doctor sliders, carousels by class; remove repeated-signature boilerplate |
| 01a | `01a-run-page-filter-editor.md` | P0 | 01 | Editable filter lists on the run page; all defaults prefilled/enabled |
| 02 | `02-sitemap-discovery-and-orphans.md` | P1 | 00 | Sitemap-assisted crawl when audit is on; content/nav reachability; sitemap-only orphans |
| 03 | `03-target-eligibility-assets-redirects-nonpages.md` | P1 | 00 | Only 200 HTML page targets count; omit assets; exclude redirect/non-page targets; no 404 duplication |
| 04 | `04-link-inventory-and-anchor-provenance.md` | P2 | 01, 03 | Full non-asset inventory, raw anchors, explicit target types |
| 05 | `05-block-evidence.md` | P2 | 01 | Signature, sample links, snippet, excluded class, repeated metadata |
| 06 | `06-occurrence-vs-unique-metrics.md` | P2 | 03, 04 | Separate occurrences from unique pairs |
| 07 | `07-pagerank-methodology.md` | P3 | 03, 06 | Document/verify PageRank; fix top-share rounding |

### Suggested sequence

- **01** first: fixes the main product behavior and removes the repeated-signature rule.
- **01a** exposes those lists on the run page, prefilled with every default.
- **02** adds sitemap coverage and reachability.
- **03** cleans the graph (assets, redirects, non-pages).
- **04/05** improve auditability.
- **06/07** add metric clarity and PageRank documentation.

### Optional sample checks

- After 01: no `recent-posts-body`/`blog-slider` targets in `result.Links`; all such blocks are `excluded`.
- After 02: Askin `/privacy-policy` and `/blog` are content-unreachable; sitemap-only pages are flagged.
- After 03: SGE reports 4 content page links, not 9; no asset rows.
- After 06: occurrences and unique pairs are both visible.

---

## 6. Definition of done

- `go test ./...` passes.
- Audit output remains deterministic for the same archive.
- Every behavior change has a synthetic-archive test.
- README accurately describes the audit scope, exclusions, sitemap behavior, and CSV columns.
- No audit-side 404/link-health duplication.
- No redirect crediting.
- No crawler changes when the audit is off.
