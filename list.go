package main

import (
	"encoding/csv"
	"fmt"
	"net/url"
	"os"
	"strings"
)

func runList(urls []string, s WizardSettings, onProgress progressFunc) ([]Page, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("no URLs to visit")
	}

	store := &pageStore{}
	starts := &requestStartTimes{}

	c := newCollectorAsync(s.Concurrency, s.ProxyURL)
	attachHTTPTiming(c, starts)
	attachPageRecording(c, starts, nil, store, s.KeywordsRaw, onProgress)

	for _, u := range urls {
		if err := c.Visit(u); err != nil {
			return nil, fmt.Errorf("visit %q: %w", u, err)
		}
	}
	c.Wait()

	return store.snapshot(), nil
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
