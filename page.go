package main

import "time"

// Page captures crawl metadata and on-page signals for export and search.
type Page struct {
	URL         string
	StatusCode  int
	LoadTime    time.Duration
	ParentURL   string
	Inlinks     string // spider: serialized inbound edges fromURL>"anchor" | ...
	Title       string
	Description string
	H1s         []string
	KeywordHits []int // occurrence counts per wizard keyword (same order as parsed "|" list)
}
