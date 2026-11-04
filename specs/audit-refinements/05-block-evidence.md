# 05 — Block Evidence Export

**Priority:** P2 (reporting and observability)
**Depends on:** 01 (class-based exclusions)
**Implement only this spec.** Do not change classification rules or link metrics here.

---

## 1. Goal

Make every block decision explainable from the exported audit files, especially the new `excluded` decision.

Add:

- block signature,
- sample links/anchors,
- snippet,
- matched exclusion class,
- repeated metadata.

Remove the now-dead related-post `content_hint` path. The comparison report's §10.4 mention of `auto_label`, `auto_excluded`, and `effective_label` does **not** apply to the current schema; do not add those fields.

---

## 2. Relevant code

| File | Function / area | Current behavior |
|---|---|---|
| `internal/audit/classify.go` | `Block` | `block_id`, tag/id/class, position, link count, link text ratio, direct anchor count, nav marker, nav score, content hint, inside content, page has content container, page count, sample pages, decision, reason. |
| `internal/audit/classify.go` | `BlockAgg` | Keeps `Signature []string`, `representative *html.Node`, `nodes []*html.Node`. |
| `internal/audit/classify.go` | `blockSignature` | Sorted canonical target URLs; `BlockID` is `blk_` + first 12 hex of SHA1. |
| `internal/audit/classify.go` | `toBlock` | Serializes aggregate features only; no signature/anchors/snippet. |
| `internal/audit/audit.go` | `writeBlocksCSV` | 16 columns; no signature/sample links/snippet. |
| `internal/audit/classify.go` | `elementText` | Can be reused for a snippet. |

---

## 3. Required behavior

### 3.1 Add block evidence fields

Add to `Block` (reuse `ExcludedClass`/`Repeated` if spec 01 already added them):

```go
Signature       string      `json:"signature,omitempty"`         // full SHA1 hex
SignatureLinks  int         `json:"signature_links,omitempty"`   // len(Signature)
ExcludedClass   string      `json:"excluded_class,omitempty"`    // matched class pattern
SampleLinks     []BlockLink `json:"sample_links,omitempty"`      // up to 5
Snippet         string      `json:"snippet,omitempty"`           // first ~200 chars
Repeated        bool        `json:"repeated,omitempty"`          // PageCount > 1
RepeatPageCount int         `json:"repeat_page_count,omitempty"` // PageCount
```

```go
type BlockLink struct {
    Target   string `json:"target"`
    Anchor   string `json:"anchor"`
    Decision string `json:"decision,omitempty"`
}
```

Guidance:

- Refactor `blockIDFor` so the same hash helper feeds `BlockID` (`blk_` + first 12) and `Signature` (full 40). Do not compute two different hashes.
- `ExcludedClass` is the pattern from spec 01 that caused the block to be excluded (e.g. `recent-posts-body`).
- `SampleLinks` comes from the representative node (highest nav-score instance, as today), sorted by target then anchor, capped at 5.
- `Snippet` is the first ~200 characters of `elementText(representative)`, whitespace-collapsed, with `...` when truncated. Do not cut mid-rune.
- `Repeated` is `PageCount > 1`. It is metadata only; spec 01 removes auto-boilerplate.

### 3.2 Remove dead content-hint path

- Spec 01 removes `relatedHintRe`/`hasRelatedPostHint` from classification.
- Remove `ContentHint` from `blockFeatures`, `BlockAgg`, `Block`, JSON, and CSV if it has no remaining consumer.
- `InsideContent` remains meaningful for normal `<main>`/`<article>` content; keep it.
- `PageHasContentContainer` remains.
- If removing `ContentHint` would be a breaking schema change for an external consumer, keep the field but always `false` and document it as deprecated. Do not leave it as a confusing “maybe true” signal.

### 3.3 Expand `audit-blocks.csv`

Append:

```text
Signature
Signature Links
Excluded Class
Sample Links
Snippet
Repeated
Repeat Page Count
```

Formatting:

- `Excluded Class` is empty for non-excluded blocks.
- `Sample Links` is compact `anchor -> target` pairs separated by ` | `, capped at 5, newlines replaced.
- `Snippet` is single-line, truncated; the CSV writer quotes it.
- Keep existing columns/order; append new ones.

`audit.json` `blocks` includes the same fields.

### 3.4 Determinism

- `SampleLinks` sorted by target, then anchor.
- `Signature` is the hash of the sorted canonical target list.
- `TestAnalyzeIsDeterministic` must pass.

---

## 4. Tests

### 4.1 Excluded block evidence

Use:

```html
<div class="section recent-posts-section">
  <div class="columns recent-posts-body">
    <a href="/a">Alpha</a>
    <a href="/b">Beta</a>
    <a href="/c">Gamma</a>
    <a href="/d">Delta</a>
    <a href="/e">Epsilon</a>
    <a href="/f">Zeta</a>
  </div>
</div>
```

Assert the block:

- `Decision == excluded`
- `ExcludedClass` contains `recent-posts`
- `Signature` is 40 hex chars
- `SignatureLinks == 6`
- `SampleLinks` has 5 entries, sorted
- `Snippet` non-empty, <= ~203 chars
- `Repeated == false` for one page

### 4.2 CSV columns

Read `audit-blocks.csv`, parse the header, assert new columns exist and `Excluded Class` is populated for the excluded block.

### 4.3 Determinism

Run `Analyze` twice and compare JSON after clearing timestamps/output paths.

### 4.4 Dead field

If `ContentHint` is removed, update tests that referenced it. If it is kept deprecated, assert it is always `false` and documented.

---

## 5. Acceptance criteria

- [ ] `audit-blocks.csv` includes signature, sample links, snippet, excluded class, and repeated metadata.
- [ ] `audit.json` block rows include the same evidence.
- [ ] Excluded blocks show the matched class.
- [ ] Dead content-hint path is removed or explicitly deprecated.
- [ ] Output remains deterministic.
- [ ] `go test ./...` passes.
- [ ] No classification policy change.

---

## 6. Out of scope

- Changing class exclusion rules (spec 01).
- Sitemap/reachability (spec 02).
- Target eligibility (spec 03).
- Link inventory columns (spec 04).
- Occurrence/unique metrics (spec 06).
- PageRank metadata (spec 07).
