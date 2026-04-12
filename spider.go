package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/gocolly/colly/v2"
)

func runSpider(s WizardSettings) ([]Page, error) {
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
	bar := newCrawlProgressBar(nil)
	defer finishCrawlProgressBar(bar, nil)

	c := newCollectorAsync(s.Concurrency)
	c.AllowedDomains = allowedHosts

	attachHTTPTiming(c, starts)
	attachPageRecording(c, starts, inbound, store, s.KeywordsRaw, bar)

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
		_ = e.Request.Visit(abs)
	})

	if err := c.Visit(start); err != nil {
		return nil, err
	}
	c.Wait()

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
