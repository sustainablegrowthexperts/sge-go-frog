# 04 — Link Inventory and Anchor Provenance

**Priority:** P2 (reporting and observability)
**Depends on:** 01 (excluded widgets), 03 (target eligibility)
**Implement only this spec.** Do not change classification rules, sitemap logic, or metrics here.

---

## 1. Goal

Make `audit-links.csv` a complete, auditable inventory of links that the audit **considered**, while keeping the internal linking graph limited to eligible HTML page links.

This spec:

- Adds external and non-HTTP rows, which are currently dropped before `Link` construction.
- Adds raw anchor text alongside the normalized anchor.
- Adds target provenance (`raw_target`, `resolved_target` is not needed for redirect crediting; use `target_type` and `decision`).
- Keeps excluded widget/redirect/non-page rows visible with reasons.
- **Does not** add asset rows. Assets are omitted entirely by spec 03.

The report's §10.1/§10.2 is partly stale: `audit-links.csv` already exports every *internal* link with `Decision`/`Reason`. The real gap is external/non-HTTP rows, raw anchors, and explicit target types.

---

## 2. Relevant code

| File | Function / area | Current behavior |
|---|---|---|
| `internal/audit/audit.go` | link loop | Early-returns for non-HTTP, invalid, external, and normalization failures; only internal HTTP links become `Link`. |
| `internal/audit/audit.go` | `Link` | `Anchor`, `Context`, parent metadata, `Rel`, `BlockID`, `Decision`, `Reason`, `Self`, `NavExcluded`. |
| `internal/audit/audit.go` | `writeLinksCSV` | Writes `result.allLinks` (internal only) with 13 columns. |
| `internal/audit/classify.go` | `anchorText` | Returns normalized text; image alt → `[image: alt]`; empty → `[image]`. |
| `internal/audit/audit.go` | `Excluded` | External/non-HTTP counts only. |
| `README.md` | audit section | Describes `audit-links.csv` as every internal link. |

---

## 3. Required behavior

### 3.1 Every non-asset anchor gets a record

Refactor the link loop so every `<a href>` except assets produces a record before graph filtering:

1. Read `href`; skip empty and `#`-only hrefs.
2. Construct the record with source, raw target, anchors, context, parent metadata, `Rel`, `Title`, and `BlockID` when available.
3. Determine target type:
   - non-HTTP scheme (`mailto:`, `tel:`, `javascript:`, `data:`, `sms:`, `ftp:`) → `non_http`.
   - external host → `external`.
   - internal target that is a 200 HTML page → `page`.
   - internal target that is a redirect alias → `redirect`.
   - internal target not in `Pages`, not an asset → `non_page`.
   - asset extension → **skip entirely** (no row, no count).
4. Apply the decision:
   - page + normal block → `content`, `nav`, `boilerplate`, `self`, or `excluded` (spec 01).
   - external → `external`.
   - non-HTTP → `non_http`.
   - redirect → `excluded` / `redirect_target`.
   - non-page → `excluded` / `target_not_page`.

Add decision constants:

```go
const (
    DecisionExternal = "external"
    DecisionNonHTTP  = "non_http"
)
```

`DecisionExcluded` is added by spec 01.

### 3.2 Raw and normalized anchors

Add to `Link`:

```go
RawAnchor  string `json:"raw_anchor,omitempty"`
AnchorKind string `json:"anchor_kind,omitempty"` // text, image_alt, image_empty
```

- `RawAnchor` preserves the original anchor text before whitespace collapsing and NBSP normalization.
- `Anchor` remains the normalized value used for analysis.
- `AnchorKind`:
  - `text` — visible text.
  - `image_alt` — image-only anchor with non-empty alt.
  - `image_empty` — image-only anchor with empty/missing alt.
- Refactor `anchorText` into a helper returning raw, normalized, and kind, or add a separate `rawAnchorText`. Do not duplicate parsing logic inconsistently.

### 3.3 Expand `audit-links.csv`

Proposed columns (keep existing columns and append):

```text
Source
Target
Raw Target
Anchor
Raw Anchor
Anchor Kind
Context
Parent Tag
Parent ID
Parent Class
Rel
Block ID
Block Signature
Decision
Reason
Target Type
Self
Nav Excluded
```

Guidance:

- `Target` is the normalized graph target (or absolute external URL / raw non-HTTP href).
- `Raw Target` is the normalized href before any target classification.
- `Target Type` is `page`, `external`, `non_http`, `redirect`, or `non_page`. There is no `asset` type because assets are omitted.
- `Block Signature` may be empty until spec 05 lands; do not block on it.
- Sort deterministically by source, target, anchor, context, decision, block ID.
- Do not include `Target Status`, `Broken`, or `Fetch Failures` columns. The main crawler owns 404s.

### 3.4 Counts reconcile

Ensure:

```text
Excluded.ExternalLinks == count(Decision == external)
Excluded.NonHTTPLinks  == count(Decision == non_http)
Excluded.ExcludedLinks == count(Decision == excluded)
Excluded.RedirectTargetLinks == count(Reason == redirect_target)
Excluded.NonPageTargetLinks  == count(Reason == target_not_page)
Excluded.SelfLinks == count(Decision == self)
Excluded.NavContainerLinks == count(Decision == nav)
```

Empty/hash-only hrefs are skipped before record creation; document that exception.

### 3.5 JSON vs CSV

- `audit.json` `links` remains content page links only.
- The full non-asset inventory is CSV-only.
- Do not serialize external/non-HTTP/excluded rows in JSON; Askin's inventory is large.
- `Result.allLinks` (or a renamed `allDiscoveredLinks`) stays unexported.

### 3.6 README/UI

- Update `README.md`: `audit-links.csv` contains every non-asset link the audit considered, with decisions and reasons.
- The existing UI link to `audit-links.csv` is enough; no new file.

---

## 4. Tests

### 4.1 Every non-asset anchor gets a row

Page with:

- 2 content page links
- 1 nav link
- 1 self link
- 1 excluded related-post link
- 1 redirect-target link
- 1 external link
- 1 `mailto:`
- 1 `.mp4` (must not appear)
- 1 `#`-only (skipped)
- 1 empty href (skipped)

Assert row counts and decisions match expectations; asset target absent from the CSV.

### 4.2 Raw vs normalized anchor

```html
<a href="/x">freckles&nbsp;and   superficial melasma</a>
```

Assert `RawAnchor` preserves the NBSP/extra spacing, `Anchor` is collapsed, `AnchorKind == "text"`.

Image cases:

```html
<a href="/y"><img alt="Laser treatment"></a>
<a href="/z"><img alt=""></a>
```

Assert `image_alt` / `image_empty` and normalized anchors.

### 4.3 Counts reconcile

Assert CSV row counts equal the `Excluded` counters for external/non-HTTP/excluded/self/nav.

### 4.4 Existing tests

- `TestAnalyzeContentGraphAndOrphans` currently checks `,self,self_link,` in the CSV; update to parse the header and inspect the `Decision`/`Reason` columns.
- `TestAnalyzeIsDeterministic` must pass.

---

## 5. Acceptance criteria

- [ ] `audit-links.csv` includes every non-asset anchor, including external/non-HTTP/excluded rows.
- [ ] Assets are absent entirely.
- [ ] Raw and normalized anchors and anchor kinds are exported.
- [ ] Target type and decision explain why a row is not in the content graph.
- [ ] Counts reconcile with CSV rows.
- [ ] `audit.json` `links` remains content page links only.
- [ ] README updated.
- [ ] `go test ./...` passes.

---

## 6. Out of scope

- Asset rows (intentionally omitted; spec 03).
- 404/target-status/fetch-failure reporting (main crawler).
- Block evidence columns (spec 05).
- Occurrence/unique metrics (spec 06).
- PageRank metadata (spec 07).
