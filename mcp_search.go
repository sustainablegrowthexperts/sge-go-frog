package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	auditpkg "go-frog/internal/audit"

	"github.com/PuerkitoBio/goquery"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// searchPattern is a compiled literal or RE2 pattern. Literal patterns are
// lowercased once when matching is case-insensitive, which keeps every scan
// comparison Unicode-aware without rebuilding the pattern per page.
type searchPattern struct {
	literal       []rune
	caseSensitive bool
	re            *regexp.Regexp
}

// compileSearchPattern validates the pattern length and compiles regex input
// with Go's linear-time RE2 engine.
func compileSearchPattern(raw string, isRegex, caseSensitive bool) (*searchPattern, error) {
	if utf8.RuneCountInString(raw) > maxSearchPatternLength {
		return nil, newToolError(codeInvalidArgument, fmt.Sprintf("pattern must be at most %d characters", maxSearchPatternLength), nil)
	}
	if isRegex {
		expr := raw
		if !caseSensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			// The regexp error can echo the pattern; the client already knows
			// it, but keep the tool error stable and free of parser details.
			return nil, newToolError(codeInvalidArgument, "pattern is not a valid RE2 regular expression", nil)
		}
		return &searchPattern{caseSensitive: caseSensitive, re: re}, nil
	}
	literal := []rune(raw)
	if !caseSensitive {
		for i := range literal {
			literal[i] = unicode.ToLower(literal[i])
		}
	}
	return &searchPattern{literal: literal, caseSensitive: caseSensitive}, nil
}

// findAll returns non-overlapping match spans as rune indexes. Empty regex
// matches are ignored so a pattern such as "a*" cannot produce an unbounded
// number of zero-width snippets.
func (p *searchPattern) findAll(text string) [][2]int {
	if p == nil || text == "" {
		return nil
	}
	if p.re != nil {
		rawMatches := p.re.FindAllStringIndex(text, -1)
		if len(rawMatches) == 0 {
			return nil
		}
		offsets := runeBoundaryOffsets(text)
		out := make([][2]int, 0, len(rawMatches))
		for _, match := range rawMatches {
			if match[0] == match[1] {
				continue
			}
			start := sort.SearchInts(offsets, match[0])
			end := sort.SearchInts(offsets, match[1])
			if start >= end {
				continue
			}
			out = append(out, [2]int{start, end})
		}
		return out
	}
	if len(p.literal) == 0 {
		return nil
	}

	runes := []rune(text)
	var out [][2]int
	for i := 0; i+len(p.literal) <= len(runes); {
		matched := true
		for j, want := range p.literal {
			got := runes[i+j]
			if !p.caseSensitive {
				got = unicode.ToLower(got)
			}
			if got != want {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, [2]int{i, i + len(p.literal)})
			i += len(p.literal)
			continue
		}
		i++
	}
	return out
}

// runeBoundaryOffsets maps every rune index to its starting byte offset, with a
// final entry for len(s), so regex byte spans can be converted without slicing
// inside a multi-byte sequence.
func runeBoundaryOffsets(text string) []int {
	offsets := make([]int, 0, utf8.RuneCountInString(text)+1)
	for i := range text {
		offsets = append(offsets, i)
	}
	return append(offsets, len(text))
}

// searchFieldText finds at most maxMatches occurrences in one field and builds
// snippets for each. It returns the number of occurrences collected and whether
// any additional occurrence was suppressed by the cap.
func searchFieldText(field, text string, pattern *searchPattern, contextChars, maxMatches int) (searchMatch, int, bool) {
	if text == "" || pattern == nil {
		return searchMatch{}, 0, false
	}
	spans := pattern.findAll(text)
	if len(spans) == 0 {
		return searchMatch{}, 0, false
	}
	if maxMatches < 0 {
		maxMatches = 0
	}
	runes := []rune(text)
	group := searchMatch{Field: field}
	if len(spans) > maxMatches {
		group.Snippets = make([]searchSnippet, 0, maxMatches)
	} else {
		group.Snippets = make([]searchSnippet, 0, len(spans))
	}
	for i, span := range spans {
		if i >= maxMatches {
			return group, group.Count, true
		}
		group.Snippets = append(group.Snippets, buildSearchSnippet(runes, span[0], span[1], contextChars))
		group.Count++
	}
	return group, group.Count, false
}

// buildSearchSnippet clamps the requested context to the field and slices by
// rune, so snippets never split a UTF-8 sequence.
func buildSearchSnippet(runes []rune, start, end, contextChars int) searchSnippet {
	if start < 0 {
		start = 0
	}
	if start > len(runes) {
		start = len(runes)
	}
	if end < start {
		end = start
	}
	if end > len(runes) {
		end = len(runes)
	}
	if contextChars < 0 {
		contextChars = 0
	}

	from := start - contextChars
	if from < 0 {
		from = 0
	}
	to := end + contextChars
	if to > len(runes) {
		to = len(runes)
	}
	return searchSnippet{
		Before: string(runes[from:start]),
		Match:  string(runes[start:end]),
		After:  string(runes[end:to]),
	}
}

// normalizeSearchScopes trims, validates, and deduplicates requested scopes.
// An omitted scope means metadata search only.
func normalizeSearchScopes(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return []string{"metadata"}, nil
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if !slices.Contains(searchScopes, entry) {
			return nil, newToolError(codeInvalidArgument, fmt.Sprintf("scope must be one of %s", strings.Join(searchScopes, ", ")), nil)
		}
		if !slices.Contains(out, entry) {
			out = append(out, entry)
		}
	}
	if len(out) == 0 {
		return []string{"metadata"}, nil
	}
	return out, nil
}

// searchScopeSelection is the expanded set of sources a request needs.
type searchScopeSelection struct {
	metadata bool
	html     bool
	text     bool
}

// selectSearchScopes expands all into the three concrete search sources.
func selectSearchScopes(scopes []string) searchScopeSelection {
	var out searchScopeSelection
	for _, scope := range scopes {
		switch scope {
		case "metadata":
			out.metadata = true
		case "html":
			out.html = true
		case "text":
			out.text = true
		case "all":
			out.metadata = true
			out.html = true
			out.text = true
		}
	}
	return out
}

// needsArchive reports whether at least one requested source requires raw HTML.
func (s searchScopeSelection) needsArchive() bool {
	return s.html || s.text
}

// searchMetadataPage searches the metadata fields available on a crawl row.
// The per-page occurrence budget is shared across all fields.
func searchMetadataPage(page Page, pattern *searchPattern, contextChars, maxMatches int) ([]searchMatch, bool) {
	fields := []struct {
		name string
		text string
	}{
		{"url", page.URL},
		{"title", page.Title},
		{"description", page.Description},
		{"robots", page.Robots},
		{"inlinks", page.Inlinks},
	}
	for _, h1 := range page.H1s {
		fields = append(fields, struct {
			name string
			text string
		}{"h1", h1})
	}
	// Consume the per-page budget in the same order the response is sorted:
	// URL, then field, then occurrence order.
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].name < fields[j].name })

	var out []searchMatch
	budget := maxMatches
	truncated := false
	for _, field := range fields {
		group, used, fieldTruncated := searchFieldText(field.name, field.text, pattern, contextChars, budget)
		if group.Count > 0 {
			group.URL = page.URL
			out = append(out, group)
			budget -= used
			if budget < 0 {
				budget = 0
			}
		}
		if fieldTruncated {
			truncated = true
			break
		}
	}
	return out, truncated
}

// searchMetadataPages searches every page row without needing an archive.
func searchMetadataPages(pages []Page, pattern *searchPattern, contextChars, maxMatches int) ([]searchMatch, bool) {
	var out []searchMatch
	truncated := false
	for _, page := range pages {
		matches, pageTruncated := searchMetadataPage(page, pattern, contextChars, maxMatches)
		out = append(out, matches...)
		if pageTruncated {
			truncated = true
		}
	}
	return out, truncated
}

// loadSearchArchive opens the job-owned archive used by html and text scopes.
func loadSearchArchive(dir string) (*auditpkg.Archive, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("archive directory is empty")
	}
	return auditpkg.LoadArchive(filepath.Clean(dir))
}

// extractSearchText returns visible text, stripping non-visible elements before
// the document's text is collected. goquery tolerates malformed HTML; the raw
// body is a safe fallback if parsing fails.
func extractSearchText(body []byte) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil || doc == nil {
		return string(body)
	}
	doc.Find("script, style, noscript, template").Remove()
	return doc.Text()
}

// truncateUTF8Bytes cuts a body at a rune boundary at or below max bytes.
func truncateUTF8Bytes(body []byte, max int) []byte {
	if max <= 0 {
		return nil
	}
	if len(body) <= max {
		return body
	}
	for max > 0 && !utf8.RuneStart(body[max]) {
		max--
	}
	return body[:max]
}

// searchArchiveEntry reads one saved body and searches the requested archive
// scopes. Read failures, including body paths that escape the archive, are
// skipped rather than exposed.
func searchArchiveEntry(archive *auditpkg.Archive, entry auditpkg.ManifestEntry, scopes searchScopeSelection, pattern *searchPattern, contextChars, maxMatches int) ([]searchMatch, bool) {
	body, err := archive.ReadBody(entry)
	if err != nil || len(body) == 0 {
		return nil, false
	}
	truncated := false
	if len(body) > maxSearchBodyBytes {
		body = truncateUTF8Bytes(body, maxSearchBodyBytes)
		truncated = true
	}

	var out []searchMatch
	budget := maxMatches
	if scopes.html {
		group, used, fieldTruncated := searchFieldText("html", string(body), pattern, contextChars, budget)
		if group.Count > 0 {
			group.URL = entry.URL
			out = append(out, group)
			budget -= used
			if budget < 0 {
				budget = 0
			}
		}
		if fieldTruncated {
			truncated = true
		}
	}
	if scopes.text {
		group, _, fieldTruncated := searchFieldText("text", extractSearchText(body), pattern, contextChars, budget)
		if group.Count > 0 {
			group.URL = entry.URL
			out = append(out, group)
		}
		if fieldTruncated {
			truncated = true
		}
	}
	return out, truncated
}

// searchArchiveEntries scans manifest entries with a bounded worker pool. Work
// is processed in URL-sorted batches and collected in batch order so results do
// not depend on goroutine scheduling. Every started worker is waited for before
// the next batch, so cancellation and early caps cannot leak goroutines.
func searchArchiveEntries(ctx context.Context, archive *auditpkg.Archive, entries []auditpkg.ManifestEntry, scopes searchScopeSelection, pattern *searchPattern, contextChars, maxMatches, totalCap int) ([]searchMatch, bool) {
	sorted := append([]auditpkg.ManifestEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].URL < sorted[j].URL })

	var out []searchMatch
	truncated := false
	for start := 0; start < len(sorted); start += searchWorkerCount {
		if ctx != nil && ctx.Err() != nil {
			truncated = true
			break
		}
		end := start + searchWorkerCount
		if end > len(sorted) {
			end = len(sorted)
		}
		batch := sorted[start:end]
		results := make([][]searchMatch, len(batch))
		flags := make([]bool, len(batch))

		var wg sync.WaitGroup
		for i := range batch {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if ctx != nil && ctx.Err() != nil {
					flags[i] = true
					return
				}
				results[i], flags[i] = searchArchiveEntry(archive, batch[i], scopes, pattern, contextChars, maxMatches)
			}(i)
		}
		wg.Wait()

		for i := range results {
			out = append(out, results[i]...)
			if flags[i] {
				truncated = true
			}
		}
		if totalCap > 0 && len(out) >= totalCap {
			if end < len(sorted) {
				truncated = true
			}
			break
		}
	}
	return out, truncated
}

// sortSearchMatches orders match groups by URL, then field, then their natural
// occurrence order for stable ties.
func sortSearchMatches(matches []searchMatch) {
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].URL != matches[j].URL {
			return matches[i].URL < matches[j].URL
		}
		return matches[i].Field < matches[j].Field
	})
}

// searchUnavailableError keeps the no-archive contract helpful without
// exposing any job-owned path.
func searchUnavailableError(jobID string) error {
	return newToolError(codeSearchNotAvailable, fmt.Sprintf("job %q has no crawl archive; start the crawl with audit=true or archive_html=true to search html, text, or all scopes", jobID), nil)
}

// searchPages searches metadata, archived HTML, or visible text for a literal
// or RE2 pattern and returns paginated match groups.
func (s *mcpServer) searchPages(ctx context.Context, _ *mcp.CallToolRequest, in searchPagesInput) (*mcp.CallToolResult, searchPagesOutput, error) {
	if err := s.checkToolRateLimit(); err != nil {
		return nil, searchPagesOutput{}, err
	}

	job, jobID, err := s.lookupMCPJob(in.JobID)
	if err != nil {
		return nil, searchPagesOutput{}, err
	}

	patternText := strings.TrimSpace(in.Pattern)
	if patternText == "" {
		return nil, searchPagesOutput{}, newToolError(codeInvalidArgument, "pattern is required", nil)
	}
	pattern, err := compileSearchPattern(patternText, in.Regex, in.CaseSensitive)
	if err != nil {
		return nil, searchPagesOutput{}, err
	}

	scopes, err := normalizeSearchScopes(in.Scope)
	if err != nil {
		return nil, searchPagesOutput{}, err
	}

	if in.Offset < 0 {
		return nil, searchPagesOutput{}, newToolError(codeInvalidArgument, "offset must be non-negative", nil)
	}
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultSearchLimit
	case limit < 0:
		return nil, searchPagesOutput{}, newToolError(codeInvalidArgument, "limit must be non-negative", nil)
	case limit > maxSearchLimit:
		limit = maxSearchLimit
	}

	contextChars := in.ContextChars
	switch {
	case contextChars == 0:
		contextChars = defaultSearchContextChars
	case contextChars < 0:
		return nil, searchPagesOutput{}, newToolError(codeInvalidArgument, "context_chars must be non-negative", nil)
	case contextChars > maxSearchContextChars:
		return nil, searchPagesOutput{}, newToolError(codeInvalidArgument, fmt.Sprintf("context_chars must be at most %d", maxSearchContextChars), nil)
	}

	maxMatches := in.MaxMatchesPerPage
	switch {
	case maxMatches == 0:
		maxMatches = defaultSearchMatchesPerPage
	case maxMatches < 0:
		return nil, searchPagesOutput{}, newToolError(codeInvalidArgument, "max_matches_per_page must be non-negative", nil)
	case maxMatches > maxSearchMatchesPerPage:
		maxMatches = maxSearchMatchesPerPage
	}

	state := job.resultsSnapshot()
	if state.status != jobStatusDone {
		return nil, searchPagesOutput{}, newToolError(codeJobNotFinished, fmt.Sprintf("job %q is %s; search is available only when the job is done", jobID, state.status), nil)
	}

	selection := selectSearchScopes(scopes)
	var archive *auditpkg.Archive
	if selection.needsArchive() {
		loaded, loadErr := loadSearchArchive(state.archiveDir)
		if loadErr != nil {
			return nil, searchPagesOutput{}, searchUnavailableError(jobID)
		}
		archive = loaded
	}

	limits := s.config.Limits.withDefaults()
	totalCap := limits.MaxSearchLimit
	if totalCap <= 0 {
		totalCap = defaultMCPMaxSearchLimit
	}

	var matches []searchMatch
	truncated := false
	if selection.metadata {
		metadataMatches, metadataTruncated := searchMetadataPages(state.pages, pattern, contextChars, maxMatches)
		matches = append(matches, metadataMatches...)
		if metadataTruncated {
			truncated = true
		}
	}
	if archive != nil && selection.needsArchive() {
		archiveBudget := totalCap - len(matches)
		if archiveBudget < 1 {
			archiveBudget = 1
		}
		archiveMatches, archiveTruncated := searchArchiveEntries(
			ctx,
			archive,
			archive.Manifest.Entries,
			selection,
			pattern,
			contextChars,
			maxMatches,
			archiveBudget,
		)
		matches = append(matches, archiveMatches...)
		if archiveTruncated {
			truncated = true
		}
	}

	sortSearchMatches(matches)
	if totalCap > 0 && len(matches) > totalCap {
		matches = matches[:totalCap]
		truncated = true
	}

	total := len(matches)
	start, end := paginateResults(total, in.Offset, limit)
	out := searchPagesOutput{
		JobID:        jobID,
		Pattern:      patternText,
		Regex:        in.Regex,
		Scope:        append([]string{}, scopes...),
		TotalMatches: total,
		Offset:       in.Offset,
		Limit:        limit,
		Truncated:    truncated,
		Matches:      make([]searchMatch, 0, end-start),
	}

	budget := int(limits.MaxResponseBytes)
	used := searchPagesResponseOverhead
	for _, match := range matches[start:end] {
		data, err := json.Marshal(match)
		if err != nil {
			return nil, searchPagesOutput{}, newToolError(codeInternalError, "could not encode a search match", map[string]any{"job_id": jobID})
		}
		if used+len(data)+1 > budget {
			out.Truncated = true
			break
		}
		used += len(data) + 1
		out.Matches = append(out.Matches, match)
	}

	out.HasMore = out.Offset+len(out.Matches) < total
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: searchPagesSummary(out)}},
	}, out, nil
}

// searchPagesSummary renders a short agent-facing summary of a search page.
func searchPagesSummary(out searchPagesOutput) string {
	if out.TotalMatches == 0 {
		return fmt.Sprintf("No matches for %q.", out.Pattern)
	}
	summary := fmt.Sprintf("Showing %d of %d match group(s) for %q.", len(out.Matches), out.TotalMatches, out.Pattern)
	if out.Truncated {
		summary += " Results were truncated at a server cap."
	}
	if out.HasMore {
		summary += fmt.Sprintf(" More matches available; next offset is %d.", out.Offset+len(out.Matches))
	}
	return summary
}
