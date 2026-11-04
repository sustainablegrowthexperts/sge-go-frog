package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const ArchiveVersion = 1

// ArchiveMeta describes one crawl archive.
type ArchiveMeta struct {
	Version    int       `json:"version"`
	StartURL   string    `json:"start_url"`
	Host       string    `json:"host"`
	Mode       string    `json:"mode"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// ManifestEntry is one fetched URL. BodyFile is only set for HTML 200
// responses; all other responses are metadata-only.
type ManifestEntry struct {
	URL           string    `json:"url"`
	StatusCode    int       `json:"status_code"`
	ContentType   string    `json:"content_type,omitempty"`
	Location      string    `json:"location,omitempty"`
	RedirectURL   string    `json:"redirect_url,omitempty"`
	XRobotsTag    []string  `json:"x_robots_tag,omitempty"`
	Depth         int       `json:"depth,omitempty"`
	BodyFile      string    `json:"body_file,omitempty"`
	BodyBytes     int64     `json:"body_bytes,omitempty"`
	DiscoveredVia string    `json:"discovered_via,omitempty"` // start, link, redirect, sitemap
	FetchedAt     time.Time `json:"fetched_at"`
}

// SitemapMeta records sitemap discovery for an audit archive. URLs holds the
// full discovered page list in sitemap.json; audit.json carries a bounded copy
// plus URLCount.
type SitemapMeta struct {
	FetchedAt time.Time `json:"fetched_at"`
	Sitemaps  []string  `json:"sitemaps,omitempty"`
	URLs      []string  `json:"urls,omitempty"`
	URLCount  int       `json:"url_count,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
}

// Manifest is written once at the end of a crawl.
type Manifest struct {
	ArchiveMeta
	Entries []ManifestEntry `json:"entries"`
}

// ArchiveWriter saves HTML bodies and response metadata without changing the
// crawler's page rows or CSV output.
type ArchiveWriter struct {
	mu      sync.Mutex
	dir     string
	meta    ArchiveMeta
	entries map[string]*ManifestEntry
	sources map[string]string
	sitemap *SitemapMeta
	closed  bool
}

// NewArchiveWriter prepares an archive directory.
func NewArchiveWriter(dir string, meta ArchiveMeta) (*ArchiveWriter, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("archive directory is empty")
	}
	if err := os.MkdirAll(filepath.Join(dir, "html"), 0o755); err != nil {
		return nil, fmt.Errorf("create archive: %w", err)
	}
	if meta.StartedAt.IsZero() {
		meta.StartedAt = time.Now().UTC()
	}
	meta.StartedAt = meta.StartedAt.UTC()
	meta.Version = ArchiveVersion
	return &ArchiveWriter{
		dir:     dir,
		meta:    meta,
		entries: make(map[string]*ManifestEntry),
		sources: make(map[string]string),
	}, nil
}

// Dir returns the archive root.
func (w *ArchiveWriter) Dir() string {
	if w == nil {
		return ""
	}
	return w.dir
}

// MarkSource records how a URL was discovered (start, link, redirect, sitemap).
// A link discovery wins over a sitemap-only discovery when both happen.
func (w *ArchiveWriter) MarkSource(rawURL, source string) {
	if w == nil || rawURL == "" || source == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if existing, ok := w.sources[rawURL]; ok && existing == "link" && source == "sitemap" {
		return
	}
	w.sources[rawURL] = source
}

// SetSitemap stores sitemap discovery metadata for sitemap.json.
func (w *ArchiveWriter) SetSitemap(meta SitemapMeta) {
	if w == nil {
		return
	}
	meta.FetchedAt = meta.FetchedAt.UTC()
	meta.Sitemaps = sortedUniqueStrings(meta.Sitemaps)
	meta.URLs = sortedUniqueStrings(meta.URLs)
	meta.URLCount = len(meta.URLs)
	meta.Warnings = append([]string(nil), meta.Warnings...)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.sitemap = &meta
}

// Record stores one response. HTML bodies are written only for 200 responses
// with an HTML content type. Non-200 responses are still recorded so the audit
// can report redirects and other response metadata.
func (w *ArchiveWriter) Record(rawURL string, statusCode int, headers *http.Header, body []byte, depth int) {
	if w == nil || rawURL == "" {
		return
	}

	contentType := ""
	location := ""
	var robots []string
	if headers != nil {
		contentType = strings.TrimSpace(headers.Get("Content-Type"))
		location = strings.TrimSpace(headers.Get("Location"))
		robots = append(robots, headers.Values("X-Robots-Tag")...)
	}

	redirectURL := ""
	if location != "" {
		if base, err := url.Parse(rawURL); err == nil {
			if ref, err := url.Parse(location); err == nil {
				redirectURL = base.ResolveReference(ref).String()
			}
		}
	}

	entry := &ManifestEntry{
		URL:         rawURL,
		StatusCode:  statusCode,
		ContentType: contentType,
		Location:    location,
		RedirectURL: redirectURL,
		XRobotsTag:  robots,
		Depth:       depth,
		FetchedAt:   time.Now().UTC(),
	}

	if statusCode == http.StatusOK && len(body) > 0 && isHTMLResponse(contentType, body) {
		name := htmlFileName(rawURL)
		if err := os.WriteFile(filepath.Join(w.dir, "html", name), body, 0o644); err == nil {
			entry.BodyFile = filepath.Join("html", name)
			entry.BodyBytes = int64(len(body))
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	entry.DiscoveredVia = w.sources[rawURL]
	if existing, ok := w.entries[rawURL]; ok {
		// Keep the richer record if the same URL is reported twice.
		if existing.BodyFile != "" && entry.BodyFile == "" {
			return
		}
		if existing.StatusCode == http.StatusOK && entry.StatusCode != http.StatusOK && existing.BodyFile != "" {
			return
		}
		if entry.DiscoveredVia == "" {
			entry.DiscoveredVia = existing.DiscoveredVia
		}
	}
	w.entries[rawURL] = entry
}

func isHTMLResponse(contentType string, body []byte) bool {
	low := strings.ToLower(contentType)
	if strings.Contains(low, "text/html") || strings.Contains(low, "application/xhtml+xml") {
		return true
	}
	if low != "" {
		return false
	}
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	text := strings.ToLower(strings.TrimSpace(string(head)))
	return strings.HasPrefix(text, "<!doctype html") ||
		strings.HasPrefix(text, "<html") ||
		strings.HasPrefix(text, "<head") ||
		strings.HasPrefix(text, "<body")
}

func htmlFileName(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])[:20] + ".html"
}

// Close writes manifest.json. It is safe to call more than once.
func (w *ArchiveWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	w.meta.FinishedAt = time.Now().UTC()

	entries := make([]ManifestEntry, 0, len(w.entries))
	for _, entry := range w.entries {
		entries = append(entries, *entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].URL < entries[j].URL })

	manifest := Manifest{ArchiveMeta: w.meta, Entries: entries}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	tmp := filepath.Join(w.dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(w.dir, "manifest.json")); err != nil {
		return fmt.Errorf("commit manifest: %w", err)
	}
	if w.sitemap != nil {
		if err := writeJSONFile(filepath.Join(w.dir, "sitemap.json"), w.sitemap); err != nil {
			return fmt.Errorf("write sitemap metadata: %w", err)
		}
	}
	return nil
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sortedUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// Archive is a loaded crawl archive.
type Archive struct {
	Dir      string
	Manifest Manifest
	Sitemap  *SitemapMeta
}

// LoadArchive reads manifest.json from dir. sitemap.json is optional so old
// archives and audit-off crawls keep loading.
func LoadArchive(dir string) (*Archive, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Version > ArchiveVersion {
		return nil, fmt.Errorf("archive version %d is newer than supported version %d", manifest.Version, ArchiveVersion)
	}
	archive := &Archive{Dir: dir, Manifest: manifest}
	sitemapData, err := os.ReadFile(filepath.Join(dir, "sitemap.json"))
	if err == nil {
		var sitemap SitemapMeta
		if err := json.Unmarshal(sitemapData, &sitemap); err != nil {
			return nil, fmt.Errorf("parse sitemap metadata: %w", err)
		}
		archive.Sitemap = &sitemap
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read sitemap metadata: %w", err)
	}
	return archive, nil
}

// ReadBody loads the HTML file for an entry and guards against path traversal.
func (a *Archive) ReadBody(entry ManifestEntry) ([]byte, error) {
	if entry.BodyFile == "" {
		return nil, nil
	}
	clean := filepath.Clean(entry.BodyFile)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return nil, fmt.Errorf("invalid body path %q", entry.BodyFile)
	}
	root, err := filepath.Abs(a.Dir)
	if err != nil {
		return nil, err
	}
	full, err := filepath.Abs(filepath.Join(root, clean))
	if err != nil {
		return nil, err
	}
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return nil, fmt.Errorf("body path escapes archive: %q", entry.BodyFile)
	}
	return os.ReadFile(full)
}
