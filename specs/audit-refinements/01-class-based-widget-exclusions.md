# 01 — Class-Based Widget Exclusions

**Priority:** P0 (product behavior)
**Depends on:** 00
**Implement only this spec.** Do not change redirect handling, sitemap discovery, assets, or reporting columns here.

---

## 1. Goal

Related posts, doctor sliders, and carousels are **not internal links** and must not count toward the internal linking audit. They should be excluded deliberately by **class matching**, not recovered as content and not inferred from content similarity or repeated signatures.

This reverses the comparison report's §4.1/§4.2 conclusion. Those blocks were described as “false negatives,” but they are intended exclusions. The current auditor is inconsistent: most `recent-posts-body` blocks are content, while three are nav. This spec makes the behavior explicit and consistent: all matching widgets are excluded.

---

## 2. Relevant code

| File | Function / area | Current behavior |
|---|---|---|
| `internal/audit/classify.go` | `relatedHintRe` | Matches related/recommended/more-from tokens and forces content. This must be removed or repurposed as an exclusion signal. |
| `internal/audit/classify.go` | `hasRelatedPostHint` | Used by `blockFeaturesForNode` and `classifyAnchor`. |
| `internal/audit/classify.go` | `blockFeaturesForNode` | Sets `ContentHint` from `hasRelatedPostHint`. |
| `internal/audit/classify.go` | `classifyBlock` | Order: override → nav marker → repeated → content hint → inside main → nav score → default. |
| `internal/audit/classify.go` | `classifyAnchor` | Currently unused; returns content for related-post hints. Remove it or align it with the new exclusion logic. |
| `internal/audit/classify.go` | `Overrides` | Existing `force_content` / `force_exclude` selectors. Add an exclusion class list alongside them. |
| `internal/audit/audit.go` | link decision loop | Only handles content/nav/boilerplate/self. |

Current Askin sample: 89 `recent-posts-body` blocks are content, 3 are nav, 1 is boilerplate. This is the inconsistency the user called out.

---

## 3. Required behavior

### 3.1 Add an `excluded` decision

Add:

```go
const (
    DecisionExcluded = "excluded"
    reasonExcludedClass = "excluded_class"
)
```

`DecisionExcluded` means: discovered, reported, but **not part of the internal link graph**. It must not count as content, nav, boilerplate, or self.

`Reason` should identify the matched class, e.g.:

```text
excluded_class:recent-posts-body
excluded_class:blog-slider
excluded_class:doctor-slider
```

### 3.2 Default substring lists

Use partial class/id matching, grouped by intent. Expose the nav/footer and feed/motion lists as package variables so the run-page UI (spec 01a) uses the same source of truth:

```go
var DefaultNavSubstrings = []string{...}
var DefaultExcludeSubstrings = []string{...}
```

**Nav containers** — classified `nav`, never content:

```text
header          (guarded, see below)
navbar
nav
menu
megamenu
meganav
subnav
drawer
offcanvas
flyout
masthead
toolbar
appbar
tabbar
sidebar
breadcrumb
```

**Footers** — classified `nav`, never content:

```text
footer
subfooter
prefooter
colophon
socket
legal
imprint
impressum
copyright
bottombar
to-top
back-to-top
```

**Related & editorial feeds** — classified `excluded`, never content:

```text
related
recommend
recirc
similar
suggest
trending
popular
latest
recent
curated
spotlight
picks
stream
grid
teaser
crosslink
cross-promo
read-next
```

**Carousels, sliders & motion** — classified `excluded`, never content:

```text
slider
slide
carousel
swiper
slick
splide
glide
flick
marquee
ticker
rotator
strip
lightbox
filmstrip
scroller
```

These cover the earlier cases: `related-posts`, `recommended-posts`, `recent-posts-body`, `blog-slider`, `new_clinic_slider`, `doctor-slider`, `doctor-carousel`, `swiper`, `slick-slider`, etc.

Matching notes:

- `related` uses a **word boundary**: `related-posts` matches; `unrelated-posts` does not.
- `recommend`, `recent`, and all other tokens are plain case-insensitive substrings. `recent` covers `recent-posts`, `recent-articles`, and `recently-viewed`.
- `carousel`, `slider`, `slide`, `swiper`, `slick`, `splide`, `glide`, `flick`, `marquee`, `ticker`, `rotator`, `strip`, `lightbox`, `filmstrip`, and `scroller` are broad. Match them on the block itself and its descendants, not on distant ancestors; see 3.4.
- `grid`, `stream`, `strip`, `slide`, `latest`, `popular`, and `picks` are broad labels. They are intentionally included, but keep them configurable so a site can remove a false positive.
- `doctor-slider` and `doctor-carousel` are covered by `slider`/`carousel`; do not add a bare `doctor` substring (it would match `doctor-profile`, which is a bio card, not necessarily a carousel).
- `new_clinic_slider` is confirmed on Askin's `/our-clinic/` page and is covered by `slider`.
- **`header` guard:** do not apply `header` as a raw substring. On Askin, `column-header`, `post-header`, and `recent-posts-header` are content headings. Match an actual `<header>` element, `role="banner"`, id `header`, or a class that **starts with** `header-`/`header_` (for example `header-container`, `header-top-bar`). Do not suffix-match `header`.
- Keep the existing `<nav>`, `<header>`, `<footer>`, `role="navigation|menu|menubar"` checks.
- Nav/footer detection runs alongside widget exclusion. A block that matches both is reported as `excluded` (the product intent is “not content” either way), but the reason should reflect the matched widget/container class.

### 3.3 Config

Extend `audit-config.json`:

```json
{
  "exclude_classes": [
    "related",
    "recommend",
    "recent",
    "carousel",
    "slider",
    "swiper",
    "slick"
  ],
  "nav_substrings": [
    "nav",
    "menu",
    "footer"
  ]
}
```

Semantics:

- `exclude_classes` appends to the built-in editorial-feed/motion defaults.
- `nav_substrings` appends to the built-in nav/footer defaults.
- A separate `replace_exclude_classes: true` / `replace_nav_substrings: true` may replace the defaults; document whichever is implemented. This is how a site removes a broad default such as `grid` or `stream`.
- Entries are partial class/id substrings, not CSS selectors.
- Normalize case, `_` vs `-`, and whitespace.
- `related` uses word-boundary matching; the rest (including `recommend` and `recent`) are plain substrings.
- A match means the matching block/subtree is excluded/nav, including all links inside it.
- Do not match generic `slider`/`slide`/`swiper`/`slick`/`strip`/`stream`/`grid` on distant ancestors; only the block itself and its descendants.

Add to `Overrides`:

```go
ExcludeClasses []string `json:"exclude_classes,omitempty"`
NavSubstrings  []string `json:"nav_substrings,omitempty"`
```

Compile them as normalized strings (not CSS selectors). Keep `force_content`/`force_exclude` behavior unchanged and higher priority than class exclusion, so a site can override a default.

Spec 01a adds a run-page editor for these lists. When the user submits explicit lists, they replace the defaults/config for that run; when they do not, the defaults/config behavior above applies.

### 3.4 Matching rules

Implement a helper, e.g.:

```go
func hasExcludedClass(n *html.Node, patterns []string) (matched string, ok bool)
```

Rules:

- Normalize `_` to `-`, lowercase, collapse whitespace.
- For each node, check `class` and `id` values.
- **Editorial feeds / motion:** match the block node itself and its descendants. `related` uses letter boundaries; `recommend`, `recent`, and the rest are plain substrings. A narrow ancestor check is allowed only for specific full widget tokens (`recent-posts`, `related`, `recommend`, `blog-slider`, `new-clinic-slider`, `doctor-slider`, `doctor-carousel`). Do not let a distant ancestor with a generic `slider`/`slide`/`swiper`/`slick`/`strip`/`stream`/`grid` class exclude an unrelated content block.
- **Nav/footer:** match on the block and its ancestors. This catches `navbar`, `submenu`, `metismenu`, `megamenu`, `meganav`, `subnav`, `drawer`, `offcanvas`, `flyout`, `masthead`, `toolbar`, `appbar`, `tabbar`, `footer-links`, `subfooter`, `prefooter`, `colophon`, `socket`, `legal`, `imprint`, `impressum`, `copyright`, `bottombar`, `to-top`, `back-to-top`, `sidebar-widget`, `breadcrumb`.
- **`header` guard:** only match an actual `<header>` element, `role="banner"`, id `header`, or a class starting with `header-`/`header_`. Do not suffix-match `header`; `column-header`/`post-header`/`recent-posts-header` are content headings.
- Return the matched pattern for the reason string.
- Keep the existing nav token/role/tag checks; the substring lists are additive, except for the `header` guard above.
- Test false positives explicitly (`unrelated-posts`, `column-header`, `post-header`, `btn-slick-prev`) and false negatives (`related-posts-body`, `metismenu`, `footer-links`, `blog-slider`, `megamenu`, `prefooter`, `splide`, `glide`, `filmstrip`, `read-next`).

### 3.5 Classification order

New order in `classifyBlock`:

```text
1. force_exclude / force_content overrides
2. excluded class (block or ancestor)       → excluded / excluded_class:<pattern>
3. nav marker                               → nav / nav_marker
4. inside strong content root               → content / inside_main
5. page has content container
   and NavScore >= 0.4                      → nav / outside_main_high_nav_score
6. no content container
   and NavScore >= 0.7                      → nav / high_nav_score_no_main
7. otherwise                                → content / default_content
```

- Remove the `relatedHintRe`/`ContentHint` **content** path. Related posts are excluded, not content.
- Remove the `len(agg.Pages) >= 2 → boilerplate` repeated-signature rule. Class matching replaces it for these widgets. Keep repetition metadata for reporting if desired, but do not use it to delete links.
- `classifyAnchor` is currently unused. Either remove it or make it apply the same exclusion logic for anchors whose block/ancestor matches an exclusion class, even when the outer block is not link-dense enough to be a block.
- Explicit nav markers no longer need to win over exclusion; both mean “not content.” Prefer `excluded` for class-matched widgets so reports show the real reason.

### 3.6 Link handling

- Links in excluded blocks get `Decision = excluded`, `Reason = excluded_class:<pattern>`.
- Excluded links must not be appended to `contentLinks`.
- Excluded links must not contribute to `ContentInbound`, `ContentOutbound`, `NavInbound`, PageRank, orphans, anchors, or over-optimization.
- Count them in a new `Excluded.ExcludedLinks` and `Excluded.ExcludedClassLinks` for reporting.
- Do not count them as nav or boilerplate.

### 3.7 Data fields

Add to `Block`:

```go
ExcludedClass string `json:"excluded_class,omitempty"`
Repeated      bool   `json:"repeated,omitempty"` // metadata only; no auto-boilerplate
```

Add to `BlockAgg`:

```go
ExcludedClass string
Repeated      bool
```

Add to `Excluded`:

```go
ExcludedLinks      int `json:"excluded_links"`
ExcludedClassLinks int `json:"excluded_class_links"`
```

Add to `Structure`:

```go
BlocksExcluded int `json:"blocks_excluded"`
```

---

## 4. Tests

### 4.1 Related posts are excluded

Two pages with:

```html
<div class="section recent-posts-section">
  <div class="columns recent-posts-body">
    <a href="/post-1">One</a>
    <a href="/post-2">Two</a>
    <a href="/post-3">Three</a>
  </div>
</div>
```

Assert:

- Block decision `excluded`, reason contains `recent-posts`.
- None of `/post-1`…`/post-3` appear in `result.Links` or inbound counts.
- `Excluded.ExcludedClassLinks >= 3`.
- Running on two pages does **not** change the decision to boilerplate.

### 4.2 Blog carousel is excluded

`ul.blog-slider` with six links → `excluded`; no targets in `result.Links`.

### 4.3 Doctor slider/carousel is excluded

Use `div.doctor-slider` and `div.doctor-carousel` fixtures. Assert excluded. If the user supplies different class names, add them to the default/config list and test those exact names.

### 4.4 `new_clinic_slider` is excluded

Use a `ul.new_clinic_slider` fixture; assert excluded.

### 4.5 Substring safety

Build fixtures for each false positive:

- `div.unrelated-posts` → **not** excluded by the `related` pattern.
- `div.column-header` and `div.post-header` → **not** nav and **not** excluded by `header`.
- A `div.wysiwyg-content` containing a nested `div.slick-slider`:
  - links inside the `slick-slider` are excluded;
  - links in the surrounding `wysiwyg-content` remain content.
- `ul.submenu.metismenu` → nav.
- `div.footer-links` → nav.

### 4.6 Grouped substring coverage

Use a table-driven test with one fixture per representative class:

| Fixture class | Expected |
|---|---|
| `megamenu`, `meganav`, `subnav`, `drawer`, `offcanvas`, `flyout`, `masthead`, `toolbar`, `appbar`, `tabbar` | `nav` |
| `subfooter`, `prefooter`, `colophon`, `socket`, `legal`, `imprint`, `impressum`, `copyright`, `bottombar`, `to-top`, `back-to-top` | `nav` |
| `recirc`, `similar`, `suggest`, `trending`, `popular`, `latest`, `recent`, `curated`, `spotlight`, `picks`, `stream`, `grid`, `teaser`, `crosslink`, `cross-promo`, `read-next` | `excluded` |
| `splide`, `glide`, `flick`, `marquee`, `ticker`, `rotator`, `strip`, `lightbox`, `filmstrip`, `scroller` | `excluded` |

Each fixture must contain at least three links so that `nearestBlock` forms a block. Assert the target links do not appear in `result.Links`.

### 4.7 Normal content is not excluded

A `<main><article>` block with body links remains `content`/`inside_main`.

### 4.8 Nav remains nav

A `<nav>`/`header` menu remains `nav`/`nav_marker`.

### 4.9 Config override

With `exclude_classes: ["my-widget"]`, a `div.my-widget` block is excluded.
With `nav_substrings: ["my-nav"]`, a `div.my-nav` block is nav.
With `force_content: [".my-widget"]`, the force-content override wins and the block is content.

### 4.10 Existing tests

- `TestRepeatedRelatedBlockIsBoilerplate` must be replaced: the block is now `excluded`, not boilerplate/content.
- `TestAnalyzeContentGraphAndOrphans` must pass; its related-posts block is now excluded, which will change expected content link counts. Update the fixture or expectations carefully. If the fixture uses `.related-posts`, it will now be excluded by design; rename it to a neutral content class if the test is meant to exercise content classification.

---

## 5. Acceptance criteria

- [ ] Related posts, blog carousels, doctor sliders, and doctor carousels are `excluded`.
- [ ] Editorial feed/motion tokens (`related`, `recommend`, `recent`, `recirc`, `similar`, `suggest`, `trending`, `popular`, `latest`, `curated`, `spotlight`, `picks`, `stream`, `grid`, `teaser`, `crosslink`, `cross-promo`, `read-next`, `slider`, `slide`, `carousel`, `swiper`, `slick`, `splide`, `glide`, `flick`, `marquee`, `ticker`, `rotator`, `strip`, `lightbox`, `filmstrip`, `scroller`) are excluded.
- [ ] Nav/footer tokens (`navbar`, `nav`, `menu`, `megamenu`, `meganav`, `subnav`, `drawer`, `offcanvas`, `flyout`, `masthead`, `toolbar`, `appbar`, `tabbar`, `footer`, `subfooter`, `prefooter`, `colophon`, `socket`, `legal`, `imprint`, `impressum`, `copyright`, `bottombar`, `to-top`, `back-to-top`, `sidebar`, `breadcrumb`) are nav.
- [ ] `header` is not used as a raw substring; `column-header`/`post-header` are not nav.
- [ ] `unrelated-posts` is not excluded.
- [ ] Nested slick/swiper controls do not wipe out surrounding content.
- [ ] Excluded links contribute to no graph metric.
- [ ] Class matching is configurable and normalized.
- [ ] The related-hint-as-content path is removed.
- [ ] The repeated-signature boilerplate rule is removed or restricted so it cannot reclassify these widgets.
- [ ] Counts and block reasons are reported.
- [ ] `go test ./...` passes.
- [ ] No sitemap, asset, redirect, or reporting changes.

---

## 6. Out of scope

- Sitemap discovery (spec 02).
- Asset and redirect target eligibility (spec 03).
- Full CSV inventory (spec 04).
- Block evidence export (spec 05).
- Occurrence/unique metrics (spec 06).
- PageRank metadata (spec 07).
