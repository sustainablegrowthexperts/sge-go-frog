package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	auditpkg "go-frog/internal/audit"

	"github.com/gocolly/colly/v2"
)

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

func attachHTTPTiming(c *colly.Collector, starts *requestStartTimes) {
	c.OnRequest(func(r *colly.Request) {
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

func attachPageRecording(c *colly.Collector, starts *requestStartTimes, inbound *inboundTracker, store *pageStore, keywordsRaw string, onProgress progressFunc, archive *auditpkg.ArchiveWriter) {
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

func newCollectorAsync(concurrency int, proxyURL string) *colly.Collector {
	c := colly.NewCollector(colly.Async(true))
	c.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	_ = c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: concurrency,
	})

	client := &http.Client{
		Timeout: 60 * time.Second,
		// Record 3xx (and other non-follow) responses as their own rows instead of following and
		// only storing the final 200 target.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// When proxyURL is set, use it explicitly. When empty, leave Transport nil so
	// Go's default http.ProxyFromEnvironment picks up HTTPS_PROXY/HTTP_PROXY env vars.
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			client.Transport = &http.Transport{
				Proxy: http.ProxyURL(u),
			}
		}
	}

	c.SetClient(client)
	return c
}
