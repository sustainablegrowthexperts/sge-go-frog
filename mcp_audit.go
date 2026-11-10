package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	auditpkg "go-frog/internal/audit"
)

// strictAuditFileName is the report written by auditpkg.Analyze into the
// strict audit output directory.
const strictAuditFileName = "audit.json"

// strictAuditCacheEntry caches a decoded report by the path it came from and
// the file metadata that identifies that version. Task 015 extends this loader
// into sectioned reads from the same cache.
type strictAuditCacheEntry struct {
	modTime time.Time
	size    int64
	result  *auditpkg.Result
}

// strictAuditCache is process-wide. Tool calls are independent and the cache
// key already includes the path, so there is no cross-job contamination.
var strictAuditCache = struct {
	sync.Mutex
	entries map[string]strictAuditCacheEntry
}{entries: make(map[string]strictAuditCacheEntry)}

// loadStrictAuditResult reads audit.json from a job-owned audit directory. The
// path never comes from a client, but it is still cleaned and rejected when it
// is empty, rooted, or attempts to escape its parent.
func loadStrictAuditResult(dir string) (*auditpkg.Result, error) {
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
		return nil, fmt.Errorf("read strict audit: %w", err)
	}

	strictAuditCache.Lock()
	entry, ok := strictAuditCache.entries[path]
	strictAuditCache.Unlock()
	if ok && entry.modTime.Equal(info.ModTime()) && entry.size == info.Size() {
		return entry.result, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read strict audit: %w", err)
	}
	var result auditpkg.Result
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode strict audit: %w", err)
	}

	strictAuditCache.Lock()
	strictAuditCache.entries[path] = strictAuditCacheEntry{
		modTime: info.ModTime(),
		size:    info.Size(),
		result:  &result,
	}
	strictAuditCache.Unlock()
	return &result, nil
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
