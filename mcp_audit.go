package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	auditpkg "go-frog/internal/audit"
)

// strictAuditFileName is the report written by auditpkg.Analyze into the
// strict audit output directory.
const strictAuditFileName = "audit.json"

// auditCacheEntry caches a decoded report by the path it came from and the
// file metadata that identifies that version. Both audit profiles share this
// cache, so a strict and an inclusive report never contend for a slot.
type auditCacheEntry struct {
	modTime time.Time
	size    int64
	result  *auditpkg.Result
}

// auditCache is process-wide. Tool calls are independent and the cache key
// already includes the path, so there is no cross-job contamination.
var auditCache = struct {
	sync.Mutex
	entries map[string]auditCacheEntry
}{entries: make(map[string]auditCacheEntry)}

// loadAuditResult reads audit.json from a job-owned audit directory. The path
// never comes from a client, but it is still cleaned and rejected when it is
// empty, rooted, or attempts to escape its parent. The decoded report is cached
// by path plus mtime/size so a rewritten report is loaded on the next call.
func loadAuditResult(dir string) (*auditpkg.Result, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("audit directory is empty")
	}
	if strings.ContainsRune(dir, 0) {
		return nil, fmt.Errorf("audit directory is invalid")
	}
	clean := filepath.Clean(dir)
	if clean == "." || clean == string(filepath.Separator) ||
		clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("audit directory is invalid")
	}
	path := filepath.Join(clean, strictAuditFileName)

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read audit: %w", err)
	}

	auditCache.Lock()
	entry, ok := auditCache.entries[path]
	auditCache.Unlock()
	if ok && entry.modTime.Equal(info.ModTime()) && entry.size == info.Size() {
		return entry.result, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read audit: %w", err)
	}
	var result auditpkg.Result
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode audit: %w", err)
	}

	auditCache.Lock()
	auditCache.entries[path] = auditCacheEntry{
		modTime: info.ModTime(),
		size:    info.Size(),
		result:  &result,
	}
	auditCache.Unlock()
	return &result, nil
}

// loadStrictAuditResult keeps the get_results join on the shared loader.
func loadStrictAuditResult(dir string) (*auditpkg.Result, error) {
	return loadAuditResult(dir)
}

// strictAuditPage returns the audit page for a crawl row by normalizing the row
// URL to the audit's graph identity. It never compares raw URL strings.
func strictAuditPage(result *auditpkg.Result, rawURL string) (*auditpkg.Page, string, bool) {
	if result == nil {
		return nil, "", false
	}
	canonical, ok := auditpkg.CanonicalURL(rawURL, nil)
	if !ok {
		return nil, "", false
	}
	page, ok := result.Pages[canonical]
	if !ok || page == nil {
		return nil, canonical, false
	}
	return page, canonical, true
}

// auditSectionSortFields is the deterministic sort surface for each paginated
// section. Sections that are not listed reject a sort field so a caller cannot
// assume an ordering that was never applied.
var auditSectionSortFields = map[string][]string{
	"pages":     {"url", "content_inbound", "pagerank", "orphan"},
	"links":     {"source", "target", "anchor"},
	"blocks":    {"block_id", "link_count", "page_count", "decision"},
	"pagerank":  {"pagerank", "inbound"},
	"redirects": {"url"},
	"failed":    {"url"},
}

var auditSectionDefaultSort = map[string]string{
	"pages":     "url",
	"links":     "source",
	"blocks":    "block_id",
	"pagerank":  "pagerank",
	"redirects": "url",
	"failed":    "url",
}

// auditSectionFilterFields lists the filter keys accepted by each section.
// Unknown keys are rejected instead of ignored.
var auditSectionFilterFields = map[string][]string{
	"pages": {
		"orphan_only", "content_unreachable_only", "sitemap_only",
		"status", "status_code", "min_pagerank",
	},
	"links": {
		"decision", "reason", "source_contains", "target_contains", "anchor_contains",
	},
	"blocks": {
		"decision", "reason", "repeated_only",
	},
	"pagerank": {"min_inbound"},
}

// validateAuditSectionArgs validates the section-specific sort and filter
// surface and returns the effective sort field.
func validateAuditSectionArgs(section, sortField string, filter map[string]any) (string, error) {
	sortField = strings.TrimSpace(sortField)
	allowedSorts, sortable := auditSectionSortFields[section]
	if sortField != "" {
		if !sortable {
			return "", newToolError(codeInvalidArgument, fmt.Sprintf("sort is not supported for section %s", section), nil)
		}
		if !slices.Contains(allowedSorts, sortField) {
			return "", newToolError(codeInvalidArgument, fmt.Sprintf("sort must be one of %s for section %s", strings.Join(allowedSorts, ", "), section), nil)
		}
	} else {
		sortField = auditSectionDefaultSort[section]
	}

	if len(filter) == 0 {
		return sortField, nil
	}
	allowedFilters, filterable := auditSectionFilterFields[section]
	if !filterable {
		return "", newToolError(codeInvalidArgument, fmt.Sprintf("filter is not supported for section %s", section), nil)
	}
	for key := range filter {
		if !slices.Contains(allowedFilters, key) {
			return "", newToolError(codeInvalidArgument, fmt.Sprintf("filter.%s is not valid for section %s", key, section), nil)
		}
	}
	return sortField, nil
}

// validateAuditFilterValues checks the value shapes for the selected section
// before any audit file is read. That keeps invalid_argument independent of
// audit readiness.
func validateAuditFilterValues(section string, filter map[string]any) error {
	if len(filter) == 0 {
		return nil
	}
	switch section {
	case "pages":
		_, err := filterAuditPages(nil, filter)
		return err
	case "links":
		_, err := filterAuditLinks(nil, filter)
		return err
	case "blocks":
		_, err := filterAuditBlocks(nil, filter)
		return err
	case "pagerank":
		_, err := filterAuditRanking(nil, filter)
		return err
	default:
		return nil
	}
}

// auditPagesFromResult flattens the audit's pages map into rows while keeping
// the graph identity as page_key.
func auditPagesFromResult(result *auditpkg.Result) []getAuditPage {
	if result == nil || len(result.Pages) == 0 {
		return []getAuditPage{}
	}
	pages := make([]getAuditPage, 0, len(result.Pages))
	for key, page := range result.Pages {
		if page == nil {
			continue
		}
		pages = append(pages, getAuditPage{
			PageKey:                    key,
			URL:                        page.URL,
			RawURL:                     page.RawURL,
			FinalURL:                   page.FinalURL,
			Depth:                      page.Depth,
			StatusCode:                 page.StatusCode,
			Title:                      page.Title,
			ContentOutbound:            page.ContentOutbound,
			ContentInbound:             page.ContentInbound,
			ContentOutboundOccurrences: page.ContentOutboundOccurrences,
			ContentInboundOccurrences:  page.ContentInboundOccurrences,
			ContentOutboundUnique:      page.ContentOutboundUnique,
			ContentInboundUnique:       page.ContentInboundUnique,
			NavInbound:                 page.NavInbound,
			PageRank:                   page.PageRank,
			IsOrphan:                   page.IsOrphan,
			OrphanType:                 page.OrphanType,
			IsStart:                    page.IsStart,
			InSitemap:                  page.InSitemap,
			SitemapOnly:                page.SitemapOnly,
			ContentOrphan:              page.ContentOrphan,
			ContentReachableFromStart:  page.ContentReachableFromStart,
			NavReachable:               page.NavReachable,
			Aliases:                    append([]string{}, page.Aliases...),
		})
	}
	return pages
}

func auditPageIdentity(page getAuditPage) string {
	if page.URL != "" {
		return page.URL
	}
	return page.PageKey
}

// filterAuditBool, filterAuditInt, filterAuditFloat, and filterAuditString each
// distinguish an absent key from a zero value and produce invalid_argument on a
// bad shape.
func filterAuditBool(filter map[string]any, key string) (bool, bool, error) {
	value, ok := filter[key]
	if !ok {
		return false, false, nil
	}
	b, ok := value.(bool)
	if !ok {
		return false, true, newToolError(codeInvalidArgument, fmt.Sprintf("filter.%s must be a boolean", key), nil)
	}
	return b, true, nil
}

func filterAuditInt(filter map[string]any, key string) (int, bool, error) {
	value, ok := filter[key]
	if !ok {
		return 0, false, nil
	}
	n, ok := statusCodeValue(value)
	if !ok {
		return 0, true, newToolError(codeInvalidArgument, fmt.Sprintf("filter.%s must be an integer", key), nil)
	}
	return n, true, nil
}

func filterAuditFloat(filter map[string]any, key string) (float64, bool, error) {
	value, ok := filter[key]
	if !ok {
		return 0, false, nil
	}
	switch v := value.(type) {
	case float64:
		return v, true, nil
	case float32:
		return float64(v), true, nil
	case int:
		return float64(v), true, nil
	case int32:
		return float64(v), true, nil
	case int64:
		return float64(v), true, nil
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, true, newToolError(codeInvalidArgument, fmt.Sprintf("filter.%s must be a number", key), nil)
		}
		return f, true, nil
	default:
		return 0, true, newToolError(codeInvalidArgument, fmt.Sprintf("filter.%s must be a number", key), nil)
	}
}

func filterAuditString(filter map[string]any, key string) (string, bool, error) {
	value, ok := filter[key]
	if !ok {
		return "", false, nil
	}
	s, ok := value.(string)
	if !ok {
		return "", true, newToolError(codeInvalidArgument, fmt.Sprintf("filter.%s must be a string", key), nil)
	}
	return strings.TrimSpace(s), true, nil
}

// filterAuditPages applies the pages filter surface. Status accepts either
// "status" or "status_code"; min_pagerank is inclusive.
func filterAuditPages(pages []getAuditPage, filter map[string]any) ([]getAuditPage, error) {
	orphanOnly, hasOrphanOnly, err := filterAuditBool(filter, "orphan_only")
	if err != nil {
		return nil, err
	}
	unreachableOnly, hasUnreachableOnly, err := filterAuditBool(filter, "content_unreachable_only")
	if err != nil {
		return nil, err
	}
	sitemapOnly, hasSitemapOnly, err := filterAuditBool(filter, "sitemap_only")
	if err != nil {
		return nil, err
	}
	status, hasStatus, err := filterAuditInt(filter, "status_code")
	if err != nil {
		return nil, err
	}
	if !hasStatus {
		status, hasStatus, err = filterAuditInt(filter, "status")
		if err != nil {
			return nil, err
		}
	}
	minRank, hasMinRank, err := filterAuditFloat(filter, "min_pagerank")
	if err != nil {
		return nil, err
	}

	out := make([]getAuditPage, 0, len(pages))
	for _, page := range pages {
		if hasOrphanOnly && page.IsOrphan != orphanOnly {
			continue
		}
		if hasUnreachableOnly && page.ContentReachableFromStart == unreachableOnly {
			continue
		}
		if hasSitemapOnly && page.SitemapOnly != sitemapOnly {
			continue
		}
		if hasStatus && page.StatusCode != status {
			continue
		}
		if hasMinRank && page.PageRank < minRank {
			continue
		}
		out = append(out, page)
	}
	return out, nil
}

// filterAuditLinks applies the content-link filter surface. String filters are
// case-insensitive substrings; decision is an exact case-insensitive match.
func filterAuditLinks(links []auditpkg.Link, filter map[string]any) ([]auditpkg.Link, error) {
	decision, hasDecision, err := filterAuditString(filter, "decision")
	if err != nil {
		return nil, err
	}
	reason, hasReason, err := filterAuditString(filter, "reason")
	if err != nil {
		return nil, err
	}
	source, hasSource, err := filterAuditString(filter, "source_contains")
	if err != nil {
		return nil, err
	}
	target, hasTarget, err := filterAuditString(filter, "target_contains")
	if err != nil {
		return nil, err
	}
	anchor, hasAnchor, err := filterAuditString(filter, "anchor_contains")
	if err != nil {
		return nil, err
	}

	out := make([]auditpkg.Link, 0, len(links))
	for _, link := range links {
		if hasDecision && !strings.EqualFold(link.Decision, decision) {
			continue
		}
		if hasReason && !strings.Contains(strings.ToLower(link.Reason), strings.ToLower(reason)) {
			continue
		}
		if hasSource && !strings.Contains(strings.ToLower(link.Source), strings.ToLower(source)) {
			continue
		}
		if hasTarget && !strings.Contains(strings.ToLower(link.Target), strings.ToLower(target)) {
			continue
		}
		if hasAnchor && !strings.Contains(strings.ToLower(link.Anchor), strings.ToLower(anchor)) {
			continue
		}
		out = append(out, link)
	}
	return out, nil
}

// filterAuditBlocks applies the block ledger filter surface.
func filterAuditBlocks(blocks []auditpkg.Block, filter map[string]any) ([]auditpkg.Block, error) {
	decision, hasDecision, err := filterAuditString(filter, "decision")
	if err != nil {
		return nil, err
	}
	reason, hasReason, err := filterAuditString(filter, "reason")
	if err != nil {
		return nil, err
	}
	repeatedOnly, hasRepeatedOnly, err := filterAuditBool(filter, "repeated_only")
	if err != nil {
		return nil, err
	}

	out := make([]auditpkg.Block, 0, len(blocks))
	for _, block := range blocks {
		if hasDecision && !strings.EqualFold(block.Decision, decision) {
			continue
		}
		if hasReason && !strings.Contains(strings.ToLower(block.Reason), strings.ToLower(reason)) {
			continue
		}
		if hasRepeatedOnly && block.Repeated != repeatedOnly {
			continue
		}
		out = append(out, block)
	}
	return out, nil
}

// filterAuditRanking applies the PageRank min_inbound threshold.
func filterAuditRanking(ranking []auditpkg.RankEntry, filter map[string]any) ([]auditpkg.RankEntry, error) {
	minInbound, hasMinInbound, err := filterAuditInt(filter, "min_inbound")
	if err != nil {
		return nil, err
	}
	if !hasMinInbound {
		return ranking, nil
	}
	out := make([]auditpkg.RankEntry, 0, len(ranking))
	for _, entry := range ranking {
		if entry.ContentInbound >= minInbound {
			out = append(out, entry)
		}
	}
	return out, nil
}

// sortAuditPages orders pages by the requested field. Every branch ties by URL
// and then page_key so map iteration order can never affect the page window.
func sortAuditPages(pages []getAuditPage, field string) {
	sort.SliceStable(pages, func(i, j int) bool {
		left, right := pages[i], pages[j]
		switch field {
		case "content_inbound":
			if left.ContentInbound != right.ContentInbound {
				return left.ContentInbound > right.ContentInbound
			}
		case "pagerank":
			if left.PageRank != right.PageRank {
				return left.PageRank > right.PageRank
			}
		case "orphan":
			if left.IsOrphan != right.IsOrphan {
				return left.IsOrphan
			}
		}
		leftURL, rightURL := auditPageIdentity(left), auditPageIdentity(right)
		if leftURL != rightURL {
			return leftURL < rightURL
		}
		return left.PageKey < right.PageKey
	})
}

// sortAuditLinks orders links with deterministic source/target/anchor ties.
func sortAuditLinks(links []auditpkg.Link, field string) {
	sort.SliceStable(links, func(i, j int) bool {
		left, right := links[i], links[j]
		cmp := 0
		switch field {
		case "target":
			cmp = strings.Compare(left.Target, right.Target)
			if cmp == 0 {
				cmp = strings.Compare(left.Source, right.Source)
			}
		case "anchor":
			cmp = strings.Compare(left.Anchor, right.Anchor)
			if cmp == 0 {
				cmp = strings.Compare(left.Source, right.Source)
			}
		default: // source
			cmp = strings.Compare(left.Source, right.Source)
			if cmp == 0 {
				cmp = strings.Compare(left.Target, right.Target)
			}
		}
		if cmp == 0 {
			cmp = strings.Compare(left.Anchor, right.Anchor)
		}
		if cmp == 0 {
			cmp = strings.Compare(left.RawTarget, right.RawTarget)
		}
		return cmp < 0
	})
}

// sortAuditBlocks orders blocks with deterministic block_id ties.
func sortAuditBlocks(blocks []auditpkg.Block, field string) {
	sort.SliceStable(blocks, func(i, j int) bool {
		left, right := blocks[i], blocks[j]
		switch field {
		case "link_count":
			if left.LinkCount != right.LinkCount {
				return left.LinkCount > right.LinkCount
			}
		case "page_count":
			if left.PageCount != right.PageCount {
				return left.PageCount > right.PageCount
			}
		case "decision":
			if left.Decision != right.Decision {
				return left.Decision < right.Decision
			}
		}
		return left.BlockID < right.BlockID
	})
}

// sortAuditRanking orders PageRank entries descending by rank or inbound count.
func sortAuditRanking(ranking []auditpkg.RankEntry, field string) {
	sort.SliceStable(ranking, func(i, j int) bool {
		left, right := ranking[i], ranking[j]
		switch field {
		case "inbound":
			if left.ContentInbound != right.ContentInbound {
				return left.ContentInbound > right.ContentInbound
			}
		default:
			if left.PageRank != right.PageRank {
				return left.PageRank > right.PageRank
			}
		}
		return left.URL < right.URL
	})
}

// applyAuditCountEnvelope reports a total for sections that expose a fixed
// aggregate rather than a paginated window.
func applyAuditCountEnvelope(out *getAuditOutput, total int) {
	out.Total = total
	out.HasMore = false
	out.NextOffset = nil
}

// applyAuditPagination writes the common pagination envelope and clamps the
// window to the filtered total.
func applyAuditPagination(out *getAuditOutput, total, offset, limit int) (start, end int) {
	out.Total = total
	out.Offset = offset
	out.Limit = limit
	start, end = paginateResults(total, offset, limit)
	out.HasMore = end < total
	if out.HasMore {
		next := end
		out.NextOffset = &next
	}
	return start, end
}

// auditSummaryFromResult builds the summary section without the report's
// out_dir field.
func auditSummaryFromResult(result *auditpkg.Result) getAuditSummary {
	if result == nil {
		return getAuditSummary{}
	}
	summary := result.Summary
	out := getAuditSummary{
		Pages:                  summary.Pages,
		ContentLinks:           summary.ContentLinks,
		ContentPageLinks:       summary.ContentPageLinks,
		ContentLinkOccurrences: summary.ContentLinkOccurrences,
		UniqueContentLinks:     summary.UniqueContentLinks,
		Orphans:                summary.Orphans,
		ContentOrphans:         summary.ContentOrphans,
		ContentUnreachable:     summary.ContentUnreachable,
		SitemapOnly:            summary.SitemapOnly,
		NavLinks:               summary.NavLinks,
		ExternalLinks:          summary.ExternalLinks,
		SelfLinks:              summary.SelfLinks,
		Links:                  len(result.Links),
		Blocks:                 len(result.Blocks),
		Redirects:              len(result.Redirects),
		Failed:                 len(result.Failed),
		TopAnchors:             len(result.Anchors.TopAnchors),
		PageRankNodes:          result.PageRankMeta.NodeCount,
		PageRankEdges:          result.PageRankMeta.EdgeCount,
		SitemapURLs:            auditSitemapCount(result.Sitemap),
	}
	if !summary.GeneratedAt.IsZero() {
		out.GeneratedAt = summary.GeneratedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func auditSitemapCount(sitemap *auditpkg.SitemapMeta) int {
	if sitemap == nil {
		return 0
	}
	if sitemap.URLCount > 0 {
		return sitemap.URLCount
	}
	return len(sitemap.URLs)
}

func auditSitemapFromResult(sitemap *auditpkg.SitemapMeta) *getAuditSitemap {
	if sitemap == nil {
		return &getAuditSitemap{}
	}
	out := &getAuditSitemap{
		URLCount: sitemap.URLCount,
		Sitemaps: append([]string{}, sitemap.Sitemaps...),
		URLs:     append([]string{}, sitemap.URLs...),
		Warnings: append([]string{}, sitemap.Warnings...),
	}
	if !sitemap.FetchedAt.IsZero() {
		out.FetchedAt = sitemap.FetchedAt.UTC().Format(time.RFC3339)
	}
	if out.URLCount == 0 {
		out.URLCount = len(sitemap.URLs)
	}
	return out
}

// auditSectionStatuses builds the "all" section availability/count table.
func auditSectionStatuses(result *auditpkg.Result) []getAuditSectionStatus {
	if result == nil {
		return []getAuditSectionStatus{}
	}
	return []getAuditSectionStatus{
		{Name: "summary", Available: true, Count: 1},
		{Name: "pages", Available: result.Pages != nil, Count: len(result.Pages)},
		{Name: "links", Available: true, Count: len(result.Links)},
		{Name: "blocks", Available: true, Count: len(result.Blocks)},
		{Name: "anchors", Available: true, Count: len(result.Anchors.TopAnchors)},
		{Name: "pagerank", Available: true, Count: len(result.PageRankRanking)},
		{Name: "redirects", Available: true, Count: len(result.Redirects)},
		{Name: "failed", Available: true, Count: len(result.Failed)},
		{Name: "sitemap", Available: result.Sitemap != nil, Count: auditSitemapCount(result.Sitemap)},
		{Name: "structure", Available: true, Count: result.Structure.TotalBlocks},
	}
}

// populateAuditSection fills only the requested section of the output.
func populateAuditSection(out *getAuditOutput, result *auditpkg.Result, sortField string, filter map[string]any) error {
	switch out.Section {
	case "summary":
		summary := auditSummaryFromResult(result)
		out.Summary = &summary
		applyAuditCountEnvelope(out, 1)
	case "pages":
		pages, err := filterAuditPages(auditPagesFromResult(result), filter)
		if err != nil {
			return err
		}
		sortAuditPages(pages, sortField)
		start, end := applyAuditPagination(out, len(pages), out.Offset, out.Limit)
		page := pages[start:end]
		out.Pages = &page
	case "links":
		links, err := filterAuditLinks(append([]auditpkg.Link{}, result.Links...), filter)
		if err != nil {
			return err
		}
		sortAuditLinks(links, sortField)
		start, end := applyAuditPagination(out, len(links), out.Offset, out.Limit)
		page := links[start:end]
		out.Links = &page
	case "blocks":
		blocks, err := filterAuditBlocks(append([]auditpkg.Block{}, result.Blocks...), filter)
		if err != nil {
			return err
		}
		sortAuditBlocks(blocks, sortField)
		start, end := applyAuditPagination(out, len(blocks), out.Offset, out.Limit)
		page := blocks[start:end]
		out.Blocks = &page
	case "anchors":
		anchors := result.Anchors
		out.Anchors = &anchors
		applyAuditCountEnvelope(out, len(result.Anchors.TopAnchors))
	case "pagerank":
		ranking, err := filterAuditRanking(append([]auditpkg.RankEntry{}, result.PageRankRanking...), filter)
		if err != nil {
			return err
		}
		sortAuditRanking(ranking, sortField)
		start, end := applyAuditPagination(out, len(ranking), out.Offset, out.Limit)
		out.PageRank = &getAuditPageRank{
			Ranking: ranking[start:end],
			Meta:    result.PageRankMeta,
		}
	case "redirects":
		redirects := append([]auditpkg.Redirect{}, result.Redirects...)
		sort.SliceStable(redirects, func(i, j int) bool { return redirects[i].URL < redirects[j].URL })
		start, end := applyAuditPagination(out, len(redirects), out.Offset, out.Limit)
		page := redirects[start:end]
		out.Redirects = &page
	case "failed":
		failed := append([]auditpkg.Failed{}, result.Failed...)
		sort.SliceStable(failed, func(i, j int) bool { return failed[i].URL < failed[j].URL })
		start, end := applyAuditPagination(out, len(failed), out.Offset, out.Limit)
		page := failed[start:end]
		out.Failed = &page
	case "sitemap":
		sitemap := auditSitemapFromResult(result.Sitemap)
		out.Sitemap = sitemap
		applyAuditCountEnvelope(out, sitemap.URLCount)
	case "structure":
		structure := result.Structure
		out.Structure = &structure
		applyAuditCountEnvelope(out, 1)
	case "all":
		summary := auditSummaryFromResult(result)
		sections := auditSectionStatuses(result)
		out.Summary = &summary
		out.Sections = &sections
		applyAuditCountEnvelope(out, len(sections))
	default:
		return newToolError(codeInvalidArgument, fmt.Sprintf("section must be one of %s", strings.Join(getAuditSections, ", ")), nil)
	}
	return nil
}

// sanitizeAuditError keeps a stored audit failure useful without echoing a
// job-owned filesystem path.
func sanitizeAuditError(text string, dirs ...string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		text = strings.ReplaceAll(text, dir, "<audit>")
	}
	return sanitizeDiagnosticText(text)
}

// getResultsAuditFromPage maps the strict audit row into the compact get_results
// audit object.
func getResultsAuditFromPage(page *auditpkg.Page, pageKey string) *getResultsAudit {
	if page == nil {
		return nil
	}
	return &getResultsAudit{
		PageKey:                    pageKey,
		ContentInbound:             page.ContentInbound,
		ContentOutbound:            page.ContentOutbound,
		ContentInboundUnique:       page.ContentInboundUnique,
		ContentOutboundUnique:      page.ContentOutboundUnique,
		ContentInboundOccurrences:  page.ContentInboundOccurrences,
		ContentOutboundOccurrences: page.ContentOutboundOccurrences,
		NavInbound:                 page.NavInbound,
		PageRank:                   page.PageRank,
		IsOrphan:                   page.IsOrphan,
		OrphanType:                 page.OrphanType,
		ContentOrphan:              page.ContentOrphan,
		ContentReachableFromStart:  page.ContentReachableFromStart,
		NavReachable:               page.NavReachable,
		InSitemap:                  page.InSitemap,
		SitemapOnly:                page.SitemapOnly,
	}
}
