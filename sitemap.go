package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	auditpkg "go-frog/internal/audit"
)

const (
	sitemapMaxDepth       = 3
	sitemapMaxBodyBytes   = 20 << 20
	sitemapRequestTimeout = 30 * time.Second
	sitemapUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

var sitemapFallbackPaths = []string{
	"/sitemap.xml",
	"/sitemap_index.xml",
	"/sitemaps.xml",
}

type sitemapDiscovery struct {
	Meta  auditpkg.SitemapMeta
	Pages []string
}

// discoverSitemap reads robots.txt and sitemap documents for the crawl site.
// Failures are collected as warnings and never abort the crawl. Every fetch
// goes through the SSRF-validated client, and non-public document or page URLs
// are skipped.
func discoverSitemap(root *url.URL, siteHost, proxyURL string, policy *ssrfPolicy) sitemapDiscovery {
	result := sitemapDiscovery{Meta: auditpkg.SitemapMeta{FetchedAt: time.Now().UTC()}}
	if root == nil {
		result.Meta.Warnings = append(result.Meta.Warnings, "sitemap discovery skipped: no start URL")
		return result
	}
	if policy == nil {
		policy = newSSRFPolicy()
	}
	client := newSSRFHTTPClient(policy, proxyURL, sitemapRequestTimeout)
	scheme := strings.ToLower(root.Scheme)
	if scheme != "http" && scheme != "https" {
		scheme = "https"
	}
	siteRoot := scheme + "://" + root.Host

	robotsURL := siteRoot + "/robots.txt"
	robotsParsed, _ := url.Parse(robotsURL)
	var sitemapURLs []string
	if err := policy.validateURL(context.Background(), robotsURL); err != nil {
		result.Meta.Warnings = append(result.Meta.Warnings, fmt.Sprintf("robots.txt target blocked by public-target policy (%s)", blockedClass(err)))
	} else if body, status, err := fetchSitemapDocument(client, robotsURL); err != nil {
		result.Meta.Warnings = append(result.Meta.Warnings, fmt.Sprintf("robots.txt fetch failed: %v", err))
	} else if status >= http.StatusOK && status < http.StatusMultipleChoices {
		for _, sitemapURL := range auditpkg.ParseRobotsSitemaps(body, robotsParsed) {
			if err := policy.validateURL(context.Background(), sitemapURL); err != nil {
				result.Meta.Warnings = append(result.Meta.Warnings, "robots.txt sitemap target blocked by public-target policy")
				continue
			}
			sitemapURLs = append(sitemapURLs, sitemapURL)
		}
	} else if status >= http.StatusInternalServerError {
		result.Meta.Warnings = append(result.Meta.Warnings, fmt.Sprintf("robots.txt returned HTTP %d", status))
	}

	if len(sitemapURLs) == 0 {
		for _, path := range sitemapFallbackPaths {
			candidate := siteRoot + path
			if err := policy.validateURL(context.Background(), candidate); err != nil {
				result.Meta.Warnings = append(result.Meta.Warnings, fmt.Sprintf("sitemap target blocked by public-target policy (%s)", blockedClass(err)))
				continue
			}
			body, status, err := fetchSitemapDocument(client, candidate)
			if err != nil {
				result.Meta.Warnings = append(result.Meta.Warnings, fmt.Sprintf("sitemap fetch failed for %s: %v", candidate, err))
				continue
			}
			if status < http.StatusOK || status >= http.StatusMultipleChoices {
				result.Meta.Warnings = append(result.Meta.Warnings, fmt.Sprintf("sitemap fetch failed for %s: HTTP %d", candidate, status))
				continue
			}
			parsed, _ := url.Parse(candidate)
			if _, parseErr := auditpkg.ParseSitemap(body, parsed); parseErr != nil {
				result.Meta.Warnings = append(result.Meta.Warnings, fmt.Sprintf("sitemap parse failed for %s: %v", candidate, parseErr))
				continue
			}
			sitemapURLs = append(sitemapURLs, candidate)
			break
		}
	}

	if len(sitemapURLs) == 0 {
		result.Meta.Warnings = append(result.Meta.Warnings, "no sitemap found")
		return result
	}

	type sitemapJob struct {
		url   string
		depth int
	}
	queue := make([]sitemapJob, 0, len(sitemapURLs))
	for _, sitemapURL := range sitemapURLs {
		if err := policy.validateURL(context.Background(), sitemapURL); err != nil {
			result.Meta.Warnings = append(result.Meta.Warnings, "sitemap target blocked by public-target policy")
			continue
		}
		queue = append(queue, sitemapJob{url: sitemapURL, depth: 1})
	}
	visited := make(map[string]struct{})
	var rawURLs []string

	for len(queue) > 0 {
		job := queue[0]
		queue = queue[1:]
		if job.depth > sitemapMaxDepth {
			continue
		}
		if _, seen := visited[job.url]; seen {
			continue
		}
		visited[job.url] = struct{}{}
		if err := policy.validateURL(context.Background(), job.url); err != nil {
			result.Meta.Warnings = append(result.Meta.Warnings, "sitemap target blocked by public-target policy")
			continue
		}

		body, status, err := fetchSitemapDocument(client, job.url)
		if err != nil {
			result.Meta.Warnings = append(result.Meta.Warnings, fmt.Sprintf("sitemap fetch failed for %s: %v", job.url, err))
			continue
		}
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			result.Meta.Warnings = append(result.Meta.Warnings, fmt.Sprintf("sitemap fetch failed for %s: HTTP %d", job.url, status))
			continue
		}
		base, _ := url.Parse(job.url)
		doc, parseErr := auditpkg.ParseSitemap(body, base)
		if parseErr != nil {
			result.Meta.Warnings = append(result.Meta.Warnings, fmt.Sprintf("sitemap parse failed for %s: %v", job.url, parseErr))
			continue
		}

		result.Meta.Sitemaps = append(result.Meta.Sitemaps, job.url)
		rawURLs = append(rawURLs, doc.URLs...)
		if job.depth < sitemapMaxDepth {
			for _, child := range doc.Sitemaps {
				if err := policy.validateURL(context.Background(), child); err != nil {
					result.Meta.Warnings = append(result.Meta.Warnings, "sitemap target blocked by public-target policy")
					continue
				}
				queue = append(queue, sitemapJob{url: child, depth: job.depth + 1})
			}
		}
	}

	result.Pages = filterPublicSitemapPages(rawURLs, siteHost, policy, &result)
	result.Meta.URLs = result.Pages
	result.Meta.URLCount = len(result.Pages)
	return result
}

// filterPublicSitemapPages applies the existing same-site/asset filter and then
// drops any discovered page whose host resolves to a non-public address.
func filterPublicSitemapPages(rawURLs []string, siteHost string, policy *ssrfPolicy, result *sitemapDiscovery) []string {
	filtered := auditpkg.FilterSitemapURLs(rawURLs, siteHost)
	out := filtered[:0]
	for _, page := range filtered {
		if err := policy.validateURL(context.Background(), page); err != nil {
			result.Meta.Warnings = append(result.Meta.Warnings, "sitemap page target blocked by public-target policy")
			continue
		}
		out = append(out, page)
	}
	return out
}

func fetchSitemapDocument(client *http.Client, rawURL string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", sitemapUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, sitemapMaxBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}
