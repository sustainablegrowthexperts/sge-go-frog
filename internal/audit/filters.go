package audit

import "strings"

// FilterOptions is a per-run override of the class-substring filter lists.
// Explicit means the caller submitted the complete effective lists, so they
// replace the defaults and any audit config for that run. When Explicit is
// false (or Filters is nil) the auditor keeps its defaults + config behavior.
type FilterOptions struct {
	NavSubstrings     []string `json:"nav_substrings,omitempty"`
	ExcludeSubstrings []string `json:"exclude_substrings"`

	Explicit bool `json:"explicit,omitempty"`
}

// SplitSubstrings turns a run-page textarea or CLI value into normalized
// patterns. Values may be separated by newlines, commas, or both. Patterns are
// trimmed, lowercased, normalized, deduplicated, and blank entries are dropped.
func SplitSubstrings(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == ','
	})
	return normalizePatterns(parts)
}

// resolveFilterPatterns returns the effective navigation and exclusion
// patterns for one audit run. An explicit run-page list replaces defaults and
// config; otherwise the existing defaults + config merge applies.
func resolveFilterPatterns(overrides *Overrides, filters *FilterOptions) (nav, exclude []string, explicit bool) {
	if filters != nil && filters.Explicit {
		return normalizePatterns(filters.NavSubstrings),
			normalizePatterns(filters.ExcludeSubstrings),
			true
	}
	return normalizePatterns(effectiveNavSubstrings(overrides)),
		normalizePatterns(effectiveExcludeClasses(overrides)),
		false
}
