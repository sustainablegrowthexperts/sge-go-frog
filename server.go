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
	"sort"
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
	owner     string // "ui" | "mcp"

	// limits and archiveHTML are immutable after creation; clientRequestID is
	// reserved for the idempotency slice.
	limits          jobLimits
	archiveHTML     bool
	clientRequestID string

	mu         sync.Mutex
	status     string // "running" | "done" | "error"
	phase      string // "starting" | "crawling" | "done" | "error"
	startedAt  time.Time
	finishedAt time.Time
	pages      int
	currentURL string
	errMsg     string
	result     []Page
	settings   WizardSettings

	truncated   bool
	limitReason string

	auditStarted bool
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

func (jm *jobManager) createWithOwner(owner, mode, target string, s WizardSettings) *crawlJob {
	var id string
	if owner == ownerMCP {
		id = newMCPJobID()
	} else {
		id = newJobID()
	}
	now := time.Now().UTC()
	j := &crawlJob{
		id:        id,
		mode:      mode,
		target:    target,
		createdAt: now,
		owner:     owner,
		status:    jobStatusRunning,
		phase:     jobPhaseStarting,
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

// jobFilter narrows list results. An empty Owner means MCP-visible jobs only.
type jobFilter struct {
	Owner  string
	Status string
	Mode   string
}

// list returns jobs visible to MCP, newest first. Browser-only jobs are hidden
// unless the caller explicitly asks for another owner.
func (jm *jobManager) list(filter jobFilter) []*crawlJob {
	owner := filter.Owner
	if owner == "" {
		owner = ownerMCP
	}

	jm.mu.RLock()
	jobs := make([]*crawlJob, 0, len(jm.jobs))
	for _, j := range jm.jobs {
		if j.owner != owner {
			continue
		}
		jobs = append(jobs, j)
	}
	jm.mu.RUnlock()

	out := jobs[:0]
	for _, j := range jobs {
		status, _ := j.statusPhase()
		if filter.Status != "" && status != filter.Status {
			continue
		}
		if filter.Mode != "" && j.mode != filter.Mode {
			continue
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].createdAt.Equal(out[k].createdAt) {
			return out[i].id < out[k].id
		}
		return out[i].createdAt.After(out[k].createdAt)
	})
	return out
}

// listPage returns one page of matching jobs and the total number of matches
// before pagination. Pagination is applied after filtering and sorting so
// total, has_more, and next_offset stay stable across calls.
func (jm *jobManager) listPage(filter jobFilter, limit, offset int) ([]*crawlJob, int) {
	jobs := jm.list(filter)
	total := len(jobs)

	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []*crawlJob{}, total
	}
	if limit <= 0 {
		limit = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return jobs[offset:end], total
}

// runningCount returns the number of running jobs for the owner, or for every
// owner when owner is empty.
func (jm *jobManager) runningCount(owner string) int {
	jm.mu.RLock()
	jobs := make([]*crawlJob, 0, len(jm.jobs))
	for _, j := range jm.jobs {
		if owner == "" || j.owner == owner {
			jobs = append(jobs, j)
		}
	}
	jm.mu.RUnlock()

	count := 0
	for _, j := range jobs {
		if status, _ := j.statusPhase(); status == jobStatusRunning {
			count++
		}
	}
	return count
}

// delete removes a job from the manager. It reports whether the job existed.
func (jm *jobManager) delete(id string) bool {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	if _, ok := jm.jobs[id]; !ok {
		return false
	}
	delete(jm.jobs, id)
	return true
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

// setPhase moves the job to an active phase and records startedAt the first
// time it becomes active.
func (j *crawlJob) setPhase(phase string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.phase = phase
	if j.startedAt.IsZero() && phase != jobPhaseDone && phase != jobPhaseError {
		j.startedAt = time.Now().UTC()
	}
}

// beginAudit marks the start of the audit pipeline so polling can distinguish
// a pending audit from one that is running.
func (j *crawlJob) beginAudit() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.auditStarted = true
}

// statusPhase returns a consistent status/phase pair for polling.
func (j *crawlJob) statusPhase() (status, phase string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status, j.phase
}

// ownedBy reports whether the job belongs to the given owner.
func (j *crawlJob) ownedBy(owner string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.owner == owner
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
	j.status = jobStatusDone
	j.phase = jobPhaseDone
	j.finishedAt = time.Now().UTC()
	j.result = pages
	j.pages = len(pages)
}

func (j *crawlJob) fail(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = jobStatusError
	j.phase = jobPhaseError
	j.finishedAt = time.Now().UTC()
	j.errMsg = err.Error()
}

func (j *crawlJob) finishWithAudits(pages []Page, strict, inclusive *auditpkg.Result, inclusiveErr error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = jobStatusDone
	j.phase = jobPhaseDone
	j.finishedAt = time.Now().UTC()
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
	j.status = jobStatusDone
	j.phase = jobPhaseDone
	j.finishedAt = time.Now().UTC()
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

// statusSnapshot copies every pollable job field under the job mutex and maps
// it to the get_job DTO. Handlers must not read mutable job fields directly.
func (j *crawlJob) statusSnapshot() getJobOutput {
	j.mu.Lock()
	defer j.mu.Unlock()

	out := getJobOutput{
		JobID:        j.id,
		Mode:         j.mode,
		Target:       j.target,
		Status:       j.status,
		Phase:        j.phase,
		CreatedAt:    rfc3339UTC(j.createdAt),
		StartedAt:    rfc3339UTC(j.startedAt),
		FinishedAt:   rfc3339UTC(j.finishedAt),
		PagesCrawled: j.pages,
		CurrentURL:   j.currentURL,
		Keywords:     splitJobKeywords(j.settings.KeywordsRaw),
		Truncated:    j.truncated,
		LimitReason:  j.limitReason,
		Limits: getJobLimits{
			MaxPages:           j.limits.MaxPages,
			MaxDepth:           j.limits.MaxDepth,
			MaxDurationSeconds: int(j.limits.MaxDuration / time.Second),
		},
		Audit: getJobAudit{
			Enabled: j.settings.Audit,
			State: auditStateFor(
				j.settings.Audit,
				j.status,
				j.auditStarted,
				j.auditReady,
				j.auditError,
				j.auditInclusiveReady,
				j.auditInclusiveError,
			),
		},
		Artifacts: getJobArtifacts{
			CSVReady:            j.status == jobStatusDone && len(j.result) > 0,
			AuditStrictReady:    j.auditReady,
			AuditInclusiveReady: j.auditInclusiveReady,
		},
	}
	if j.settings.Audit {
		if j.auditReady || j.auditError != "" {
			out.Audit.Strict = &getJobAuditProfile{Ready: j.auditReady, Error: j.auditError}
		}
		if j.auditInclusiveReady || j.auditInclusiveError != "" {
			out.Audit.Inclusive = &getJobAuditProfile{Ready: j.auditInclusiveReady, Error: j.auditInclusiveError}
		}
	}
	if j.errMsg != "" {
		msg := j.errMsg
		out.Error = &msg
	}
	return out
}

// summarySnapshot maps the job's full status snapshot to the compact list_jobs
// view so both tools share one field mapping.
func (j *crawlJob) summarySnapshot() jobSummary {
	return jobSummaryFromStatus(j.statusSnapshot())
}

// rfc3339UTC formats a timestamp for the polling DTO, or nil when unset.
func rfc3339UTC(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// splitJobKeywords returns the job's parsed keyword list, never nil.
func splitJobKeywords(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	parts := strings.Split(raw, "|")
	keywords := make([]string, 0, len(parts))
	for _, part := range parts {
		if keyword := strings.TrimSpace(part); keyword != "" {
			keywords = append(keywords, keyword)
		}
	}
	return keywords
}

// auditStateFor maps a consistent job snapshot to one audit state value. Audit
// failures stay separate from crawl status.
func auditStateFor(enabled bool, status string, started, strictReady bool, strictErr string, inclusiveReady bool, inclusiveErr string) string {
	if !enabled {
		return auditStateDisabled
	}
	if strictErr != "" {
		return auditStateError
	}
	if strictReady {
		switch {
		case inclusiveErr != "":
			return auditStatePartial
		case inclusiveReady:
			return auditStateReady
		}
	}
	if status == jobStatusRunning {
		if started {
			return auditStateRunning
		}
		return auditStatePending
	}
	if status == jobStatusDone {
		// A completed crawl with the audit enabled produced no report and no
		// stored audit error: surface that as an audit failure.
		return auditStateError
	}
	return auditStatePending
}

func newJobID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// newMCPJobID returns a 128-bit, lower-case hex job ID for MCP-created jobs.
func newMCPJobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; fall back to the UI
		// shape rather than returning an empty ID.
		return newJobID()
	}
	return hex.EncodeToString(b[:])
}

// ── application wiring ───────────────────────────────────────────────────────

// appConfig holds the settings needed to build the shared application: the web
// UI routes and, when enabled, the MCP endpoint on the same mux.
type appConfig struct {
	MCP MCPConfig
	// Runner overrides the real crawl runner. Tests use a fake runner; a nil
	// value selects the production implementation.
	Runner crawlRunner
}

type app struct {
	jobs    *jobManager
	runner  crawlRunner
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

	runner := cfg.Runner
	if runner == nil {
		runner = realCrawlRunner{}
	}

	jm := newJobManager()
	mux := http.NewServeMux()
	registerWebRoutes(mux, jm, runner)

	a := &app{jobs: jm, runner: runner, handler: mux}
	if cfg.MCP.Enabled {
		ms, err := newMCPServer(jm, cfg.MCP, runner)
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

func registerWebRoutes(mux *http.ServeMux, jm *jobManager, runner crawlRunner) {
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

		job := jm.createWithOwner(ownerUI, mode, target, settings)
		go runCrawlJob(job, settings, runner)

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

// runCrawlJob is the single launch path shared by the web UI and the MCP
// tools. It runs the crawler and then, when enabled, the strict/inclusive
// audit pipeline.
func runCrawlJob(job *crawlJob, settings WizardSettings, runner crawlRunner) {
	job.setPhase(jobPhaseCrawling)

	var pages []Page
	var err error
	switch settings.Mode {
	case 1:
		pages, err = runner.Spider(context.Background(), settings, job.progress)
	case 2:
		pages, err = runner.List(context.Background(), settings.listURLs, settings, job.progress)
	default:
		err = fmt.Errorf("unknown mode %d", settings.Mode)
	}
	if err != nil {
		job.fail(err)
		return
	}

	if settings.Audit {
		job.beginAudit()
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
	return explicitAuditFilters(
		auditpkg.SplitSubstrings(r.FormValue("audit_nav_substrings")),
		auditpkg.SplitSubstrings(r.FormValue("audit_exclude_substrings")),
	)
}

// explicitAuditFilters builds the replacement profile shared by the UI filter
// editor and the MCP audit_filters input. Callers pass already-split lists.
func explicitAuditFilters(nav, exclude []string) *auditpkg.FilterOptions {
	return &auditpkg.FilterOptions{
		NavSubstrings:     append([]string(nil), nav...),
		ExcludeSubstrings: append([]string(nil), exclude...),
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
