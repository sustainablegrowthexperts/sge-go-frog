package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	auditpkg "go-frog/internal/audit"

	"github.com/gocolly/colly/v2"
)

func runSpider(s WizardSettings, onProgress progressFunc) ([]Page, error) {
	start, err := normalizeHTTPURL(s.StartURL)
	if err != nil {
		return nil, fmt.Errorf("starting URL: %w", err)
	}

	root, err := url.Parse(start)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	host := strings.ToLower(root.Hostname())
	if host == "" {
		return nil, fmt.Errorf("URL has no host")
	}
	allowedHosts := expandedWWWHostVariants(host)

	inbound := newInboundTracker()
	store := &pageStore{}
	starts := &requestStartTimes{}

	var archive *auditpkg.ArchiveWriter
	var sitemapPages []string
	if s.Audit {
		archiveDir := strings.TrimSpace(s.AuditArchiveDir)
		if archiveDir == "" {
			archiveDir = buildArchiveDirForHost(host, time.Now())
		}
		archive, err = auditpkg.NewArchiveWriter(archiveDir, auditpkg.ArchiveMeta{
			StartURL: start,
			Host:     host,
			Mode:     "spider",
		})
		if err != nil {
			return nil, fmt.Errorf("create audit archive: %w", err)
		}
		defer archive.Close()

		discovery := discoverSitemap(root, host, s.ProxyURL)
		archive.SetSitemap(discovery.Meta)
		sitemapPages = discovery.Pages
	}

	c := newCollectorAsync(s.Concurrency, s.ProxyURL)
	c.AllowedDomains = allowedHosts

	attachHTTPTiming(c, starts)
	attachPageRecording(c, starts, inbound, store, s.KeywordsRaw, onProgress, archive)

	// Colly does not run OnResponse/OnScraped when it treats 3xx as an HTTP error, so enqueue
	// redirect targets from OnError (see scrape.go) instead of OnResponse.
	c.OnError(func(r *colly.Response, _ error) {
		if r == nil || r.Request == nil || r.Headers == nil {
			return
		}
		if r.StatusCode < 301 || r.StatusCode > 308 {
			return
		}
		loc := strings.TrimSpace(r.Headers.Get("Location"))
		if loc == "" {
			return
		}
		abs := r.Request.AbsoluteURL(loc)
		if abs == "" {
			return
		}
		u, err := url.Parse(abs)
		if err != nil {
			return
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return
		}
		if !hostMatchesSite(u.Hostname(), host) {
			return
		}
		if archive != nil {
			archive.MarkSource(abs, "redirect")
		}
		_ = r.Request.Visit(abs)
	})

	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		href := strings.TrimSpace(e.Attr("href"))
		if href == "" || strings.HasPrefix(href, "#") {
			return
		}
		abs := e.Request.AbsoluteURL(href)
		if abs == "" {
			return
		}
		u, err := url.Parse(abs)
		if err != nil {
			return
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return
		}
		if !hostMatchesSite(u.Hostname(), host) {
			return
		}
		anchor := strings.TrimSpace(e.Text)
		inbound.add(abs, e.Request.URL.String(), anchor)
		if archive != nil {
			archive.MarkSource(abs, "link")
		}
		_ = e.Request.Visit(abs)
	})

	if archive != nil {
		archive.MarkSource(start, "start")
	}
	if err := c.Visit(start); err != nil {
		return nil, err
	}
	c.Wait()

	// Sitemap coverage runs after link-following so we only fetch pages the
	// crawl could not reach through links. The collector records them exactly
	// like any other page, including redirects and 404s.
	if s.Audit && len(sitemapPages) > 0 {
		seen := make(map[string]struct{})
		for _, page := range store.snapshot() {
			if canonical, ok := auditpkg.CanonicalURL(page.URL, nil); ok {
				seen[canonical] = struct{}{}
			}
		}
		for _, sitemapURL := range sitemapPages {
			if canonical, ok := auditpkg.CanonicalURL(sitemapURL, nil); ok {
				if _, done := seen[canonical]; done {
					continue
				}
			}
			if visited, _ := c.HasVisited(sitemapURL); visited {
				continue
			}
			if archive != nil {
				archive.MarkSource(sitemapURL, "sitemap")
			}
			_ = c.Visit(sitemapURL)
		}
		c.Wait()
	}

	if archive != nil {
		if err := archive.Close(); err != nil {
			return nil, fmt.Errorf("finalize audit archive: %w", err)
		}
	}

	return store.snapshot(), nil
}

func normalizeHTTPURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("missing host")
	}
	u.Fragment = ""
	return u.String(), nil
}

// expandedWWWHostVariants allows apex <-> www for the same site (e.g. example.com vs www.example.com).
func expandedWWWHostVariants(host string) []string {
	host = strings.ToLower(host)
	set := map[string]struct{}{host: {}}
	if strings.HasPrefix(host, "www.") {
		set[strings.TrimPrefix(host, "www.")] = struct{}{}
	} else {
		set["www."+host] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	return out
}

func stripWWW(h string) string {
	h = strings.ToLower(h)
	return strings.TrimPrefix(h, "www.")
}

func hostMatchesSite(linkHost, startHost string) bool {
	a, b := strings.ToLower(linkHost), strings.ToLower(startHost)
	if a == b {
		return true
	}
	return stripWWW(a) == stripWWW(b)
}
