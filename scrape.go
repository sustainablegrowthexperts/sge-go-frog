package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	auditpkg "go-frog/internal/audit"

	"github.com/gocolly/colly/v2"
)

// Crawl limit reasons reported through truncated outcomes. The values are part
// of the MCP get_job contract.
const (
	limitReasonMaxPages    = "max_pages"
	limitReasonMaxDepth    = "max_depth"
	limitReasonMaxDuration = "max_duration"
	limitReasonMaxListURLs = "max_list_urls"
)

// crawlBudget enforces the per-run caps shared by spider and list mode and
// remembers the first cap reached. Every method is safe for concurrent colly
// callbacks; page counting and recording are checked under one mutex so the
// recorded row count can never exceed max_pages. A nil budget is unbounded.
type crawlBudget struct {
	mu sync.Mutex

	maxPages    int
	maxDepth    int
	hasMaxDepth bool
	deadline    time.Time

	seen      map[string]struct{}
	pages     int
	truncated bool
	reason    string
}

func newCrawlBudget(limits *jobLimits, deadline time.Time) *crawlBudget {
	b := &crawlBudget{seen: make(map[string]struct{}), deadline: deadline}
	if limits != nil {
		b.maxPages = limits.MaxPages
		b.maxDepth = limits.MaxDepth
		b.hasMaxDepth = true
	}
	return b
}

func (b *crawlBudget) markLocked(reason string) {
	if b.truncated {
		return
	}
	b.truncated = true
	b.reason = reason
}

// setReason records a cap that was detected outside the recording path.
func (b *crawlBudget) setReason(reason string) {
	if b == nil || reason == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.markLocked(reason)
}

// stopForWork reports whether new work must not be scheduled because the page
// or duration cap was reached. The depth cap is per-branch and is handled by
// allowLink.
func (b *crawlBudget) stopForWork() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.deadline.IsZero() && !time.Now().Before(b.deadline) {
		b.markLocked(limitReasonMaxDuration)
		return true
	}
	if b.maxPages > 0 && b.pages >= b.maxPages {
		b.markLocked(limitReasonMaxPages)
		return true
	}
	return false
}

// allowLink reports whether links found on a page at parentDepth may be
// followed. max_depth counts link hops from the start page (depth 0), while
// Colly numbers the start request itself as depth 1; a link found on a request
// at parentDepth is therefore allowed when parentDepth <= maxDepth.
// Sitemap-coverage pages are outside the link frontier and do not call this
// helper.
func (b *crawlBudget) allowLink(parentDepth int) bool {
	if b == nil {
		return true
	}
	if b.stopForWork() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hasMaxDepth && parentDepth > b.maxDepth {
		b.markLocked(limitReasonMaxDepth)
		return false
	}
	return true
}

// allowPage reserves a recording slot for a distinct URL. It returns false when
// the page cap is exhausted; the caller must then skip the row instead of
// exceeding the cap. The check and the count increment share the budget mutex,
// so concurrent callbacks can reserve at most max_pages slots between them.
func (b *crawlBudget) allowPage(u string) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxPages > 0 && b.pages >= b.maxPages {
		b.markLocked(limitReasonMaxPages)
		return false
	}
	if _, ok := b.seen[u]; ok {
		return true
	}
	b.seen[u] = struct{}{}
	b.pages++
	return true
}

// outcome snapshots the recorded pages and truncation metadata.
func (b *crawlBudget) outcome(pages []Page) crawlOutcome {
	if b == nil {
		return crawlOutcome{Pages: pages}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return crawlOutcome{Pages: pages, Truncated: b.truncated, LimitReason: b.reason}
}

// pageStore collects Page rows from concurrent colly callbacks.
type pageStore struct {
	mu    sync.Mutex
	pages []Page
}

func (s *pageStore) append(p Page) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pages = append(s.pages, p)
}

func (s *pageStore) snapshot() []Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Page, len(s.pages))
	copy(out, s.pages)
	return out
}

// requestStartTimes tracks wall time per colly request ID (Ctx is shared across branches).
type requestStartTimes struct {
	mu sync.Map // uint32 -> time.Time
}

func (t *requestStartTimes) mark(id uint32) {
	t.mu.Store(id, time.Now())
}

func (t *requestStartTimes) take(id uint32) (time.Time, bool) {
	v, ok := t.mu.LoadAndDelete(id)
	if !ok {
		return time.Time{}, false
	}
	ts, ok := v.(time.Time)
	return ts, ok
}

// attachHTTPTiming records request start times and aborts queued requests once
// the page or duration cap is reached. Aborting in OnRequest happens before the
// HTTP round-trip, so no row, progress event, or archive entry is produced for
// work scheduled past a cap.
func attachHTTPTiming(c *colly.Collector, starts *requestStartTimes, budget *crawlBudget) {
	c.OnRequest(func(r *colly.Request) {
		if budget.stopForWork() {
			r.Abort()
			return
		}
		starts.mark(r.ID)
	})
}

// openCrawlArchive prepares the shared crawl archive writer used by spider and
// list mode. An empty directory is rejected here so a job never runs with an
// implicit archive location.
func openCrawlArchive(dir string, meta auditpkg.ArchiveMeta) (*auditpkg.ArchiveWriter, error) {
	archive, err := auditpkg.NewArchiveWriter(strings.TrimSpace(dir), meta)
	if err != nil {
		return nil, fmt.Errorf("create crawl archive: %w", err)
	}
	return archive, nil
}

// closeCrawlArchive finalizes the manifest exactly once and surfaces write
// errors to the caller. A nil writer is a no-op.
func closeCrawlArchive(archive *auditpkg.ArchiveWriter) error {
	if archive == nil {
		return nil
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("finalize crawl archive: %w", err)
	}
	return nil
}

func attachPageRecording(c *colly.Collector, starts *requestStartTimes, inbound *inboundTracker, store *pageStore, budget *crawlBudget, keywordsRaw string, onProgress progressFunc, archive *auditpkg.ArchiveWriter) {
	// OnScraped runs once per request after a successful HTTP round-trip (including non-2xx
	// bodies that Colly still parses). Callback/HTML errors after a 2xx still reach OnScraped.
	c.OnScraped(func(r *colly.Response) {
		t0, ok := starts.take(r.Request.ID)
		var elapsed time.Duration
		if ok {
			elapsed = time.Since(t0)
		}
		body := append([]byte(nil), r.Body...)
		u := r.Request.URL.String()
		if !budget.allowPage(u) {
			return
		}
		if archive != nil {
			archive.Record(u, r.StatusCode, r.Headers, body, r.Request.Depth)
		}
		title, description, h1s, keywordHits, imgAlts, robots := pageFieldsForExport(r.StatusCode, body, keywordsRaw, u, r.Headers)
		parentURL, inlinks := pageParentAndInlinks(inbound, u)
		store.append(Page{
			URL:         u,
			StatusCode:  r.StatusCode,
			LoadTime:    elapsed,
			ParentURL:   parentURL,
			Inlinks:     inlinks,
			Title:       title,
			Description: description,
			H1s:         h1s,
			ImgAlts:     imgAlts,
			Robots:      robots,
			KeywordHits: keywordHits,
		})
		if onProgress != nil {
			onProgress(u)
		}
	})

	c.OnError(func(resp *colly.Response, err error) {
		if resp == nil || resp.Request == nil {
			// Connection-level failures (DNS, dial timeout, TLS).
			if onProgress != nil {
				onProgress(fmt.Sprintf("ERR:%v", err))
			}
			return
		}
		// For 2xx, Colly already ran OnScraped. Colly then calls OnError for later issues (e.g. HTML
		// parsing). Do not append a second row. For 3xx/4xx, the first handleOnError aborts fetch
		// before OnScraped, so those rows are recorded only here.
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			return
		}
		req := resp.Request
		t0, ok := starts.take(req.ID)
		var elapsed time.Duration
		if ok {
			elapsed = time.Since(t0)
		}
		body := append([]byte(nil), resp.Body...)
		u := req.URL.String()
		if !budget.allowPage(u) {
			return
		}
		if archive != nil {
			// Metadata is kept for redirects and errors; the writer only saves
			// HTML bodies for 200 responses.
			archive.Record(u, resp.StatusCode, resp.Headers, body, req.Depth)
		}
		title, description, h1s, keywordHits, imgAlts, robots := pageFieldsForExport(resp.StatusCode, body, keywordsRaw, u, resp.Headers)
		parentURL, inlinks := pageParentAndInlinks(inbound, u)
		store.append(Page{
			URL:         u,
			StatusCode:  resp.StatusCode,
			LoadTime:    elapsed,
			ParentURL:   parentURL,
			Inlinks:     inlinks,
			Title:       title,
			Description: description,
			H1s:         h1s,
			ImgAlts:     imgAlts,
			Robots:      robots,
			KeywordHits: keywordHits,
		})
		if onProgress != nil {
			onProgress(u)
		}
	})
}

func pageParentAndInlinks(inbound *inboundTracker, pageURL string) (parentURL, inlinks string) {
	if inbound == nil {
		return "", ""
	}
	return inbound.parentAndInlinks(pageURL)
}

func newCollectorAsync(concurrency int, proxyURL string, policy *ssrfPolicy) *colly.Collector {
	c := colly.NewCollector(colly.Async(true))
	c.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	_ = c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: concurrency,
	})

	client := newSSRFHTTPClient(policy, proxyURL, 60*time.Second)
	// Record 3xx (and other non-follow) responses as their own rows instead of following and
	// only storing the final 200 target.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	c.SetClient(client)
	return c
}
