package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	auditpkg "go-frog/internal/audit"
)

func runList(urls []string, s WizardSettings, onProgress progressFunc) ([]Page, error) {
	outcome, err := runListContext(context.Background(), urls, s, onProgress)
	return outcome.Pages, err
}

// runListContext is the context-aware list entry point. It deduplicates the
// input, enforces the list-URL cap before any visit, and applies the same page
// and duration caps as spider mode.
func runListContext(ctx context.Context, urls []string, s WizardSettings, onProgress progressFunc) (outcome crawlOutcome, err error) {
	if len(urls) == 0 {
		return crawlOutcome{}, fmt.Errorf("no URLs to visit")
	}

	urls = dedupeURLs(urls)
	listCapHit := false
	if s.limits != nil && s.limits.MaxListURLs > 0 && len(urls) > s.limits.MaxListURLs {
		urls = urls[:s.limits.MaxListURLs]
		listCapHit = true
	}

	policy := s.ssrfGuard()
	for _, u := range urls {
		if err := policy.validateURL(context.Background(), u); err != nil {
			return crawlOutcome{}, fmt.Errorf("blocked target: %w", err)
		}
	}

	if s.limits != nil && s.limits.MaxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.limits.MaxDuration)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()
	budget := newCrawlBudget(s.limits, deadline)
	if listCapHit {
		budget.setReason(limitReasonMaxListURLs)
	}

	store := &pageStore{}
	starts := &requestStartTimes{}

	var archive *auditpkg.ArchiveWriter
	if s.ArchiveHTML || s.Audit {
		archiveDir := strings.TrimSpace(s.AuditArchiveDir)
		if archiveDir == "" {
			archiveDir = buildArchiveDir(s, time.Now())
		}
		meta := auditpkg.ArchiveMeta{
			StartURL: urls[0],
			Mode:     "list",
		}
		if u, parseErr := url.Parse(urls[0]); parseErr == nil {
			meta.Host = u.Hostname()
		}
		archive, err = openCrawlArchive(archiveDir, meta)
		if err != nil {
			return crawlOutcome{}, err
		}
		defer func() {
			if closeErr := closeCrawlArchive(archive); closeErr != nil && err == nil {
				err = closeErr
				outcome = crawlOutcome{}
			}
		}()
	}

	c := newCollectorAsync(s.Concurrency, s.ProxyURL, policy)
	attachHTTPTiming(c, starts, budget)
	attachPageRecording(c, starts, nil, store, budget, s.KeywordsRaw, onProgress, archive)

	for _, u := range urls {
		if budget.stopForWork() {
			break
		}
		if err := c.Visit(u); err != nil {
			return crawlOutcome{}, fmt.Errorf("visit %q: %w", u, err)
		}
	}
	c.Wait()

	if ctx.Err() != nil && !deadline.IsZero() {
		budget.setReason(limitReasonMaxDuration)
	}
	return budget.outcome(store.snapshot()), nil
}

// dedupeURLs preserves the first occurrence of each URL.
func dedupeURLs(urls []string) []string {
	seen := make(map[string]struct{}, len(urls))
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

func urlsFromCSV(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open CSV: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true

	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read CSV: %w", err)
	}

	seen := make(map[string]struct{})
	var out []string
	for _, row := range records {
		for _, cell := range row {
			cell = strings.TrimSpace(cell)
			if cell == "" {
				continue
			}
			if u, ok := parseHTTPLike(cell); ok {
				if _, dup := seen[u]; dup {
					continue
				}
				seen[u] = struct{}{}
				out = append(out, u)
			}
		}
	}
	return out, nil
}

func parseHTTPLike(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	u.Fragment = ""
	return u.String(), true
}
