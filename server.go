package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	auditpkg "go-frog/internal/audit"
	"go-frog/internal/auditbundle"
)

//go:embed web/*
var webFS embed.FS

var indexTmpl = template.Must(template.ParseFS(webFS, "web/index.html"))

// ── job manager ──────────────────────────────────────────────────────────────

type jobManager struct {
	mu   sync.RWMutex
	jobs map[string]*crawlJob
}

type crawlJob struct {
	id        string
	mode      string // "spider" | "list"
	target    string
	createdAt time.Time

	mu         sync.Mutex
	status     string // "running" | "done" | "error"
	pages      int
	currentURL string
	errMsg     string
	result     []Page
	settings   WizardSettings

	auditReady   bool
	auditError   string
	auditSummary auditpkg.AuditSummary
	auditDir     string

	auditInclusiveReady   bool
	auditInclusiveError   string
	auditInclusiveSummary auditpkg.AuditSummary
	auditInclusiveDir     string
}

func newJobManager() *jobManager {
	jm := &jobManager{jobs: make(map[string]*crawlJob)}
	go jm.reapLoop()
	return jm
}

func (jm *jobManager) create(mode, target string, s WizardSettings) *crawlJob {
	j := &crawlJob{
		id:        newJobID(),
		mode:      mode,
		target:    target,
		createdAt: time.Now(),
		status:    "running",
		settings:  s,
	}
	jm.mu.Lock()
	jm.jobs[j.id] = j
	jm.mu.Unlock()
	return j
}

func (jm *jobManager) get(id string) *crawlJob {
	jm.mu.RLock()
	defer jm.mu.RUnlock()
	return jm.jobs[id]
}

func (jm *jobManager) reapLoop() {
	for range time.Tick(5 * time.Minute) {
		jm.mu.Lock()
		cutoff := time.Now().Add(-30 * time.Minute)
		for id, j := range jm.jobs {
			if j.createdAt.Before(cutoff) {
				delete(jm.jobs, id)
			}
		}
		jm.mu.Unlock()
	}
}

func (j *crawlJob) progress(url string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pages++
	j.currentURL = url
}

func (j *crawlJob) finish(pages []Page) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = "done"
	j.result = pages
	j.pages = len(pages)
}

func (j *crawlJob) fail(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = "error"
	j.errMsg = err.Error()
}

func (j *crawlJob) finishWithAudits(pages []Page, strict, inclusive *auditpkg.Result, inclusiveErr error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = "done"
	j.result = pages
	j.pages = len(pages)
	j.auditReady = true
	j.auditError = ""
	j.auditSummary = strict.SummaryValue()
	j.auditDir = strict.OutputDir

	j.auditInclusiveReady = inclusive != nil
	j.auditInclusiveError = ""
	j.auditInclusiveSummary = auditpkg.AuditSummary{}
	j.auditInclusiveDir = ""
	switch {
	case inclusive != nil:
		j.auditInclusiveSummary = inclusive.SummaryValue()
		j.auditInclusiveDir = inclusive.OutputDir
	case inclusiveErr != nil:
		j.auditInclusiveError = inclusiveErr.Error()
	}
}

func (j *crawlJob) finishWithAuditError(pages []Page, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = "done"
	j.result = pages
	j.pages = len(pages)
	j.auditReady = false
	j.auditError = err.Error()
}

type jobView struct {
	ID         string
	Mode       string
	Target     string
	Status     string
	Pages      int
	CurrentURL string
	Error      string

	Audit        bool
	AuditReady   bool
	AuditError   string
	AuditPages   int
	AuditLinks   int
	AuditOrphans int

	AuditInclusiveReady   bool
	AuditInclusiveError   string
	AuditInclusivePages   int
	AuditInclusiveLinks   int
	AuditInclusiveOrphans int
}

func (j *crawlJob) view() jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	u := j.currentURL
	if len(u) > 80 {
		u = u[:77] + "..."
	}
	return jobView{
		ID:           j.id,
		Mode:         j.mode,
		Target:       j.target,
		Status:       j.status,
		Pages:        j.pages,
		CurrentURL:   u,
		Error:        j.errMsg,
		Audit:        j.settings.Audit,
		AuditReady:   j.auditReady,
		AuditError:   j.auditError,
		AuditPages:   j.auditSummary.Pages,
		AuditLinks:   j.auditSummary.ContentLinkOccurrences,
		AuditOrphans: j.auditSummary.Orphans,

		AuditInclusiveReady:   j.auditInclusiveReady,
		AuditInclusiveError:   j.auditInclusiveError,
		AuditInclusivePages:   j.auditInclusiveSummary.Pages,
		AuditInclusiveLinks:   j.auditInclusiveSummary.ContentLinkOccurrences,
		AuditInclusiveOrphans: j.auditInclusiveSummary.Orphans,
	}
}

func newJobID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ── application wiring ───────────────────────────────────────────────────────

// appConfig holds the settings needed to build the shared application: the web
// UI routes and, when enabled, the MCP endpoint on the same mux.
type appConfig struct {
	MCP MCPConfig
}

type app struct {
	jobs    *jobManager
	mcp     *mcpServer
	handler http.Handler
}

// newApp builds the job manager and HTTP mux used by both the web UI and the
// MCP endpoint. MCP is opt-in; when disabled, routing is exactly the previous
// web-only behavior.
func newApp(cfg appConfig) (*app, error) {
	if cfg.MCP.Enabled {
		if err := cfg.MCP.validate(); err != nil {
			return nil, err
		}
	}

	jm := newJobManager()
	mux := http.NewServeMux()
	registerWebRoutes(mux, jm)

	a := &app{jobs: jm, handler: mux}
	if cfg.MCP.Enabled {
		ms, err := newMCPServer()
		if err != nil {
			return nil, fmt.Errorf("initialize MCP server: %w", err)
		}
		mux.Handle(cfg.MCP.Path, ms.Handler())
		a.mcp = ms
	}

	return a, nil
}

func (a *app) Handler() http.Handler {
	return a.handler
}

// Close releases application resources. It must be safe to call after the HTTP
// listener has stopped.
func (a *app) Close(ctx context.Context) error {
	if a.mcp != nil {
		return a.mcp.Close(ctx)
	}
	return nil
}

// serve starts the shared application on the given address.
func serve(addr string, cfg MCPConfig) error {
	a, err := newApp(appConfig{MCP: cfg})
	if err != nil {
		return err
	}
	defer a.Close(context.Background())

	fmt.Printf("go-frog web server listening on %s\n", addr)
	return http.ListenAndServe(addr, a.Handler())
}

// ── http server ──────────────────────────────────────────────────────────────

func registerWebRoutes(mux *http.ServeMux, jm *jobManager) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		indexTmpl.Execute(w, map[string]any{
			"NavSubstrings":     strings.Join(auditpkg.DefaultNavSubstrings, "\n"),
			"ExcludeSubstrings": strings.Join(auditpkg.DefaultExcludeSubstrings, "\n"),
		})
	})

	mux.HandleFunc("/api/crawl", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		mode := r.FormValue("mode")
		settings := WizardSettings{
			KeywordsRaw: strings.TrimSpace(r.FormValue("keywords")),
		}
		if c := r.FormValue("concurrency"); c != "" {
			if n, err := strconv.Atoi(c); err == nil && n > 0 {
				settings.Concurrency = n
			}
		}
		if settings.Concurrency == 0 {
			settings.Concurrency = defaultConcurrency
		}

		var target string

		switch mode {
		case "spider":
			settings.Mode = 1
			settings.StartURL = strings.TrimSpace(r.FormValue("startUrl"))
			if settings.StartURL == "" {
				http.Error(w, "Starting URL is required", http.StatusBadRequest)
				return
			}
			target = settings.StartURL
		case "list":
			settings.Mode = 2
			raw := strings.TrimSpace(r.FormValue("urls"))
			if raw == "" {
				http.Error(w, "URLs are required", http.StatusBadRequest)
				return
			}
			var listURLs []string
			seen := make(map[string]struct{})
			for _, line := range strings.Split(raw, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if u, ok := parseHTTPLike(line); ok {
					if _, dup := seen[u]; !dup {
						seen[u] = struct{}{}
						listURLs = append(listURLs, u)
					}
				}
			}
			if len(listURLs) == 0 {
				http.Error(w, "No valid URLs found", http.StatusBadRequest)
				return
			}
			settings.listURLs = listURLs
			target = fmt.Sprintf("%d URLs", len(listURLs))
		default:
			http.Error(w, "invalid mode", http.StatusBadRequest)
			return
		}

		// Internal linking audit is opt-in and spider-only.
		if mode == "spider" && r.FormValue("audit") == "1" {
			settings.Audit = true
			settings.AuditArchiveDir = buildArchiveDir(settings, time.Now())
			settings.AuditFilters = parseAuditFilters(r, mode, true)
		}

		job := jm.create(mode, target, settings)

		go func() {
			var pages []Page
			var err error

			switch settings.Mode {
			case 1:
				pages, err = runSpider(settings, func(url string) {
					job.progress(url)
				})
			case 2:
				pages, err = runList(settings.listURLs, settings, func(url string) {
					job.progress(url)
				})
			}

			if err != nil {
				job.fail(err)
				return
			}

			if settings.Audit {
				result, auditErr := auditpkg.Analyze(settings.AuditArchiveDir, auditpkg.Options{Filters: settings.AuditFilters})
				if auditErr != nil {
					job.finishWithAuditError(pages, fmt.Errorf("internal linking audit: %w", auditErr))
					return
				}

				// Build the inclusive report from the same archive. It keeps the
				// strict run's navigation filters but counts every other
				// widget/listing link as content.
				inclusive, inclusiveErr := auditpkg.Analyze(settings.AuditArchiveDir, auditpkg.Options{
					Filters: inclusiveAuditFilters(result),
					OutDir:  filepath.Join(settings.AuditArchiveDir, "inclusive"),
				})
				job.finishWithAudits(pages, result, inclusive, inclusiveErr)
				return
			}
			job.finish(pages)
		}()

		w.Header().Set("Content-Type", "text/html")
		writeJobFragment(w, job.view())
	})

	mux.HandleFunc("/api/crawl/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/crawl/")
		parts := strings.SplitN(path, "/", 2)
		id := parts[0]

		job := jm.get(id)
		if job == nil {
			http.NotFound(w, r)
			return
		}

		if len(parts) == 2 && parts[1] == "download" {
			serveDownload(w, r, job)
			return
		}

		if len(parts) == 2 && strings.HasPrefix(parts[1], "audit/") {
			switch strings.TrimPrefix(parts[1], "audit/") {
			case "bundle.zip":
				serveAuditBundle(w, r, job, "strict")
			case "inclusive/bundle.zip":
				serveAuditBundle(w, r, job, "inclusive")
			default:
				http.NotFound(w, r)
			}
			return
		}

		w.Header().Set("Content-Type", "text/html")
		writeJobFragment(w, job.view())
	})
}

// inclusiveAuditFilters returns the filter profile for the less strict report:
// the same navigation/menu/footer patterns as the strict run and no widget or
// listing exclusions. A nil or partial strict result falls back to the default
// navigation list so the inclusive run never accidentally counts site chrome.
func inclusiveAuditFilters(strict *auditpkg.Result) *auditpkg.FilterOptions {
	nav := append([]string(nil), auditpkg.DefaultNavSubstrings...)
	if strict != nil && strict.FiltersUsed != nil {
		nav = append([]string(nil), strict.FiltersUsed.NavSubstrings...)
	}
	return &auditpkg.FilterOptions{
		NavSubstrings:     nav,
		ExcludeSubstrings: nil,
		Explicit:          true,
	}
}

// parseAuditFilters reads the run-page filter editor fields. Filters are only
// accepted for spider-mode runs with the audit enabled, and only replace the
// defaults when the client confirms it submitted the complete lists. Absent
// fields leave AuditFilters nil so the auditor uses defaults + config.
func parseAuditFilters(r *http.Request, mode string, auditEnabled bool) *auditpkg.FilterOptions {
	if !auditEnabled || mode != "spider" {
		return nil
	}
	if r.FormValue("audit_filters_explicit") != "1" {
		return nil
	}
	return &auditpkg.FilterOptions{
		NavSubstrings:     auditpkg.SplitSubstrings(r.FormValue("audit_nav_substrings")),
		ExcludeSubstrings: auditpkg.SplitSubstrings(r.FormValue("audit_exclude_substrings")),
		Explicit:          true,
	}
}

func serveDownload(w http.ResponseWriter, r *http.Request, job *crawlJob) {
	job.mu.Lock()
	pages := job.result
	settings := job.settings
	status := job.status
	errMsg := job.errMsg
	job.mu.Unlock()

	if status == "error" {
		http.Error(w, errMsg, http.StatusInternalServerError)
		return
	}

	path := filepath.Join(os.TempDir(), buildResultsFilename(settings, time.Now()))
	if err := writeResultsCSV(path, pages, settings.KeywordsRaw); err != nil {
		http.Error(w, "failed to generate CSV", http.StatusInternalServerError)
		return
	}
	defer os.Remove(path)

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(path)))
	http.ServeFile(w, r, path)
}

// serveAuditBundle packages the profile's report directory into one ZIP and
// sends it as the only audit download exposed by the web UI.
func serveAuditBundle(w http.ResponseWriter, r *http.Request, job *crawlJob, profile string) {
	job.mu.Lock()
	dir := job.auditDir
	ready := job.auditReady
	settings := job.settings
	if profile == "inclusive" {
		dir = job.auditInclusiveDir
		ready = job.auditInclusiveReady
	}
	job.mu.Unlock()

	if !ready {
		http.NotFound(w, r)
		return
	}

	tmp, err := os.CreateTemp(os.TempDir(), "go-frog-audit-*.zip")
	if err != nil {
		http.Error(w, "failed to create audit bundle", http.StatusInternalServerError)
		return
	}
	bundlePath := tmp.Name()
	tmp.Close()
	os.Remove(bundlePath)
	defer os.Remove(bundlePath)

	if err := auditbundle.WriteFile(dir, bundlePath); err != nil {
		fmt.Fprintf(os.Stderr, "build audit bundle: %v\n", err)
		http.Error(w, "failed to build audit bundle", http.StatusInternalServerError)
		return
	}

	filename := sanitizeFileSlug(resultFileSlug(settings)) + "-" + profile + "-audit.zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	http.ServeFile(w, r, bundlePath)
}

// ── html fragment rendering ──────────────────────────────────────────────────

func writeJobFragment(w io.Writer, v jobView) {
	// Opening tag — add hx-get polling if running.
	fmt.Fprintf(w, `<div class="job" id="job-%s"`, v.ID)
	if v.Status == "running" {
		fmt.Fprintf(w, ` hx-get="/api/crawl/%s" hx-trigger="every 500ms" hx-swap="outerHTML"`, v.ID)
	}
	fmt.Fprint(w, `>`)

	// Header
	fmt.Fprintf(w, `<div class="job-head"><span class="job-mode">%s</span><span class="job-target" title="%s">%s</span></div>`,
		strings.ToUpper(v.Mode), esc(v.Target), esc(v.Target))

	// Progress bar
	switch v.Status {
	case "done":
		fmt.Fprint(w, `<div class="job-bar"><div class="job-fill done" style="width:100%"></div></div>`)
	case "error":
		fmt.Fprint(w, `<div class="job-bar"><div class="job-fill err" style="width:100%"></div></div>`)
	default:
		fmt.Fprint(w, `<div class="job-bar"><div class="job-fill" style="width:50%"></div></div>`)
	}

	// Info line
	fmt.Fprint(w, `<div class="job-info">`)
	switch v.Status {
	case "running":
		fmt.Fprintf(w, `<span>%d pages</span><span class="job-url">%s</span>`, v.Pages, esc(v.CurrentURL))
	case "done":
		fmt.Fprintf(w, `<span>%d pages &#10003;</span>`, v.Pages)
		fmt.Fprintf(w, `<a class="btn-dl" href="/api/crawl/%s/download">&#x2B07; Download CSV</a>`, v.ID)
	case "error":
		fmt.Fprintf(w, `<span class="err-text">%s</span>`, esc(v.Error))
	}
	fmt.Fprint(w, `</div>`)

	if v.Audit {
		switch {
		case v.AuditReady:
			fmt.Fprint(w, `<div class="job-audit" x-data="{ auditTab: 'strict' }">`)
			fmt.Fprint(w, `<div class="job-audit-head"><span>Internal linking audit</span><div class="audit-tabs">`)
			fmt.Fprint(w, `<button type="button" class="audit-tab" :class="{ active: auditTab === 'strict' }" @click="auditTab = 'strict'">Strict</button>`)
			fmt.Fprint(w, `<button type="button" class="audit-tab" title="Counts every link outside navigation, menus, and footers" :class="{ active: auditTab === 'inclusive' }" @click="auditTab = 'inclusive'">Inclusive</button>`)
			fmt.Fprint(w, `</div></div>`)
			fmt.Fprint(w, `<div x-show="auditTab === 'strict'">`)
			writeAuditPanel(w, true, "", v.AuditPages, v.AuditLinks, v.AuditOrphans, "/api/crawl/"+v.ID+"/audit", "")
			fmt.Fprint(w, `</div>`)
			fmt.Fprint(w, `<div x-show="auditTab === 'inclusive'" x-cloak>`)
			writeAuditPanel(w, v.AuditInclusiveReady, v.AuditInclusiveError, v.AuditInclusivePages, v.AuditInclusiveLinks, v.AuditInclusiveOrphans, "/api/crawl/"+v.ID+"/audit/inclusive", "Inclusive audit did not produce a report.")
			fmt.Fprint(w, `</div></div>`)
		case v.AuditError != "":
			fmt.Fprintf(w, `<div class="job-audit"><div class="job-audit-head">Internal linking audit failed</div><div class="err-text">%s</div></div>`, esc(v.AuditError))
		case v.Status == "done":
			fmt.Fprint(w, `<div class="job-audit"><div class="job-audit-head">Internal linking audit</div><div class="job-audit-stats">Audit did not produce a report.</div></div>`)
		}
	}

	fmt.Fprint(w, `</div>`)
}

func writeAuditPanel(w io.Writer, ready bool, errText string, pages, links, orphans int, baseURL, emptyMsg string) {
	if !ready {
		if errText != "" {
			fmt.Fprintf(w, `<div class="job-audit-stats err-text">%s</div>`, esc(errText))
		} else if emptyMsg != "" {
			fmt.Fprintf(w, `<div class="job-audit-stats">%s</div>`, emptyMsg)
		}
		return
	}
	orphanWord := "orphans"
	if orphans == 1 {
		orphanWord = "orphan"
	}
	fmt.Fprintf(w, `<div class="job-audit-stats">%d pages &middot; %d content link occurrences &middot; %d %s</div>`,
		pages, links, orphans, orphanWord)
	fmt.Fprint(w, `<div class="job-audit-links">`)
	fmt.Fprintf(w, `<a href="%s/bundle.zip">&#x2B07; Download audit bundle (ZIP)</a>`, baseURL)
	fmt.Fprint(w, `</div>`)
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}
