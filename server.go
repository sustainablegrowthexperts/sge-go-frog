package main

import (
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

type jobView struct {
	ID         string
	Mode       string
	Target     string
	Status     string
	Pages      int
	CurrentURL string
	Error      string
}

func (j *crawlJob) view() jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	u := j.currentURL
	if len(u) > 80 {
		u = u[:77] + "..."
	}
	return jobView{
		ID:         j.id,
		Mode:       j.mode,
		Target:     j.target,
		Status:     j.status,
		Pages:      j.pages,
		CurrentURL: u,
		Error:      j.errMsg,
	}
}

func newJobID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ── http server ──────────────────────────────────────────────────────────────

func startServer(addr string) error {
	jm := newJobManager()
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		indexTmpl.Execute(w, nil)
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
			} else {
				job.finish(pages)
			}
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

		w.Header().Set("Content-Type", "text/html")
		writeJobFragment(w, job.view())
	})

	fmt.Printf("go-frog web server listening on %s\n", addr)
	return http.ListenAndServe(addr, mux)
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
	fmt.Fprint(w, `</div></div>`)
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}
