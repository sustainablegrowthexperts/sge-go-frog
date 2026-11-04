# 01a — Run-Page Filter Editor

**Priority:** P0 (usability)
**Depends on:** 01 (class-based widget exclusions)
**Implement only this spec.** Do not change the matching rules, sitemap, target eligibility, or reporting here.

**Precondition:** spec 01 must already be implemented (`DecisionExcluded`, `DefaultNavSubstrings`, `DefaultExcludeSubstrings`, exclusion matching, and config fields). If those do not exist in the codebase, stop and ask the user to run spec 01 first. Do not partially re-implement spec 01 inside this thread.

---

## 1. Goal

Let the user edit the class-substring filter lists from the crawl run page, with **all default patterns enabled on page load**.

The run page already has an “Perform internal linking audit?” checkbox. When it is checked, show an editable filter section containing the navigation/footer list and the excluded-widget list, pre-filled with every default. Removing a line disables that pattern for the run; adding a line enables a custom pattern.

This is a per-run override. Existing defaults remain the source of truth when the fields are absent (CLI, API callers, old clients).

---

## 2. Relevant code

| File | Function / area | Current behavior |
|---|---|---|
| `web/index.html` | crawl form | Alpine state `{mode, keywords}`; audit checkbox with `name="audit"`; form posts multipart to `/api/crawl`. |
| `web/index.html` | audit field | Only a checkbox and hint; no filter controls. |
| `server.go` | `startServer` `/` handler | `indexTmpl.Execute(w, nil)`; no template data. |
| `server.go` | `/api/crawl` handler | Parses `mode`, `keywords`, `concurrency`, `startUrl`/`urls`, `audit`; sets `settings.Audit` and `AuditArchiveDir`. |
| `main.go` | `WizardSettings` | Has `Audit bool`, `AuditArchiveDir`; no filter fields. |
| `internal/audit/audit.go` | `Options` | `OutDir`, `ConfigPath`; no runtime filters. |
| `internal/audit/classify.go` | `Overrides` | Has `ForceContent`, `ForceExclude`; spec 01 adds `ExcludeClasses`/`NavSubstrings`. |
| `internal/audit/audit.go` | `Analyze` | Loads `Overrides` from config; no per-run filter override. |

---

## 3. Required behavior

### 3.1 UI

Add to `web/index.html`, inside the audit field (visible only when `mode === 'spider'` and the audit checkbox is checked):

```text
☑ Perform internal linking audit?
  [ Edit class filters ▾ ]

  Navigation & footer class filters
  [ textarea, prefilled with all nav/footer defaults ]

  Excluded widget class filters
  [ textarea, prefilled with all feed/motion defaults ]

  [Reset navigation list] [Reset excluded list]
  Hint: case-insensitive partial class/id matches. Remove a line to disable it.
```

Requirements:

- The root Alpine component gains `audit: false` and uses `x-model="audit"` on the checkbox.
- The filter block uses `x-show="mode==='spider' && audit"` with `x-cloak`.
- Both textareas are **pre-filled with the complete default lists**, so every default is enabled by default. (If the implementing thread prefers chips/checkboxes, every default must start checked; the textarea approach is simpler and is the recommended baseline.)
- One pattern per line. Also accept comma-separated input when parsing.
- Include a “Reset to defaults” control per list. It restores the server-provided default text without submitting (e.g. store the default in a `data-default` attribute and restore with Alpine `$refs`).
- Include a short hint explaining partial matching and the `header` guard.
- Do not show the filters in List mode. If audit is unchecked, do not submit the fields: use `:disabled="!audit"` on the textareas and hidden input, or render the block with `x-if` so it leaves the DOM.
- Keep the existing visual style (`.field`, `.hint`, `.check-row`).

Suggested form field names:

```text
audit_nav_substrings
audit_exclude_substrings
audit_filters_explicit=1
```

`audit_filters_explicit` tells the server that the user saw and submitted the complete lists. Without it (old client/API), the server uses defaults + config.

### 3.2 Template defaults

Expose the default lists from the audit package so the UI and backend cannot drift:

```go
// internal/audit/classify.go or filters.go
var DefaultNavSubstrings = []string{...}
var DefaultExcludeSubstrings = []string{...}
```

In `server.go` `/` handler:

```go
indexTmpl.Execute(w, map[string]any{
    "NavSubstrings":     strings.Join(auditpkg.DefaultNavSubstrings, "\n"),
    "ExcludeSubstrings": strings.Join(auditpkg.DefaultExcludeSubstrings, "\n"),
})
```

`web/index.html` renders them into the textareas:

```html
<textarea name="audit_nav_substrings" rows="12">{{.NavSubstrings}}</textarea>
<textarea name="audit_exclude_substrings" rows="12">{{.ExcludeSubstrings}}</textarea>
```

Use `html/template` escaping correctly; the values are plain text.

If the template is executed with `nil` elsewhere, guard with empty defaults or always pass the map.

### 3.3 Settings and audit options

Add to `WizardSettings`:

```go
AuditFilters *auditpkg.FilterOptions
```

Add to `auditpkg`:

```go
type FilterOptions struct {
    NavSubstrings     []string `json:"nav_substrings,omitempty"`
    ExcludeSubstrings []string `json:"exclude_substrings,omitempty"`
    Explicit          bool     `json:"explicit,omitempty"`
}
```

Add to `auditpkg.Options`:

```go
Filters *FilterOptions
```

In `server.go` `/api/crawl`:

- Parse `audit_nav_substrings` and `audit_exclude_substrings` with a shared splitter:
  - split on newlines and commas,
  - trim whitespace,
  - drop blanks,
  - lowercase/normalize,
  - dedupe while preserving order.
- If `audit_filters_explicit == "1"` and audit is enabled, set `settings.AuditFilters` with `Explicit: true`. The submitted lists are the complete effective lists.
- If the fields are absent, leave `AuditFilters` nil; the auditor uses defaults + config as today.
- Ignore filters when audit is off or mode is list.
- Pass `Filters: settings.AuditFilters` to `auditpkg.Analyze`.

### 3.4 Merge precedence

In `auditpkg.Analyze`:

```text
if opts.Filters != nil && opts.Filters.Explicit:
    effective nav = opts.Filters.NavSubstrings
    effective exclude = opts.Filters.ExcludeSubstrings
else:
    effective nav = defaults + config.NavSubstrings
    effective exclude = defaults + config.ExcludeSubstrings
```

- Run-page explicit lists **replace** defaults and config for that run. This is necessary so removing a line actually disables a default.
- If the user clears a textarea, the effective list is empty (all filters disabled). This is intentional but should be visible in the UI hint.
- If not explicit, preserve the existing config merge behavior from spec 01.
- Normalize patterns once before classification.

### 3.5 Reproducibility

Add to `auditpkg.Result`:

```go
FiltersUsed *FilterOptions `json:"filters_used,omitempty"`
```

- Populate with the effective nav/exclude lists and `Explicit`.
- This lets a saved audit explain why a block was excluded or counted, even if the UI defaults change later.
- Do not dump the lists into every block; one top-level field is enough.

### 3.6 CLI / API compatibility

- `cmd/audit` continues to use defaults + `--config` when no filters are passed.
- Optional: add `--nav-substrings` and `--exclude-substrings` comma-separated flags for parity. Not required for this spec.
- Old clients that post only `audit=1` keep current default behavior.

---

## 4. Tests

### 4.1 Defaults are prefilled

- Render `/` (or execute `indexTmpl` with the map) and assert:
  - `audit_nav_substrings` contains every `DefaultNavSubstrings` entry.
  - `audit_exclude_substrings` contains every `DefaultExcludeSubstrings` entry.
  - All default tokens are present, so all are enabled by default.

### 4.2 Parse run-page filters

Unit-test the splitter:

```text
"related, recommend\ncarousel\n\n slider "
→ ["related","recommend","carousel","slider"]
```

Assert dedupe, trim, lowercase/normalization, and blank removal.

### 4.3 Explicit list replaces defaults

Call `Analyze` with `Options{Filters: &FilterOptions{ExcludeSubstrings: []string{"related"}, Explicit: true}}` on a page with `related-posts` and a `carousel` block:

- `related-posts` is excluded.
- `carousel` is **not** excluded by default (because the explicit list replaced defaults), so it follows normal classification.

### 4.4 Empty explicit list disables filters

Call `Analyze` with explicit empty lists on a `related-posts` block. Assert it is not excluded by class (it may still be nav by heuristics; check the decision/reason).

### 4.5 Non-explicit keeps defaults + config

Call `Analyze` with `Filters: nil` and the existing `audit-config.json`. Assert defaults and config are merged as before.

### 4.6 Audit off / list mode

Post the form with `audit` unchecked or `mode=list`; assert `AuditFilters` is nil and filters do not affect the crawl.

### 4.7 `filters_used`

Assert `audit.json` contains `filters_used` with the effective lists when explicit, and the merged lists when not.

---

## 5. Acceptance criteria

- [ ] The run page shows editable nav/footer and excluded-widget filter lists when audit is enabled.
- [ ] Both lists are prefilled with every default, so all are enabled by default.
- [ ] Removing a line disables that pattern for the run.
- [ ] Adding a line enables a custom pattern.
- [ ] “Reset to defaults” restores the complete lists.
- [ ] Explicit run-page lists replace defaults/config; absent fields preserve current behavior.
- [ ] `filters_used` is saved in `audit.json`.
- [ ] Audit-off and list-mode behavior are unchanged.
- [ ] `go test ./...` passes.
- [ ] No matching-rule or classification changes.

---

## 6. Out of scope

- Changing the default patterns or matching rules (spec 01).
- Persisting filters across runs (optional localStorage can be added later).
- Sitemap (spec 02).
- Target eligibility (spec 03).
- Reporting columns (specs 04/05).
- Metrics/PageRank (specs 06/07).
