package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	auditpkg "go-frog/internal/audit"
)

func TestIndexTemplatePrefillsAuditFilters(t *testing.T) {
	var buf bytes.Buffer
	err := indexTmpl.Execute(&buf, map[string]any{
		"NavSubstrings":     strings.Join(auditpkg.DefaultNavSubstrings, "\n"),
		"ExcludeSubstrings": strings.Join(auditpkg.DefaultExcludeSubstrings, "\n"),
	})
	if err != nil {
		t.Fatalf("execute template: %v", err)
	}
	out := buf.String()

	for _, field := range []string{
		`name="audit_nav_substrings"`,
		`name="audit_exclude_substrings"`,
		`name="audit_filters_explicit"`,
	} {
		if !strings.Contains(out, field) {
			t.Fatalf("run page is missing %s", field)
		}
	}

	navDefaults := strings.Join(auditpkg.DefaultNavSubstrings, "\n")
	if !strings.Contains(out, navDefaults) {
		t.Fatalf("navigation textarea is not prefilled with every default:\n%s", navDefaults)
	}
	excludeDefaults := strings.Join(auditpkg.DefaultExcludeSubstrings, "\n")
	if !strings.Contains(out, excludeDefaults) {
		t.Fatalf("excluded textarea is not prefilled with every default:\n%s", excludeDefaults)
	}
	for _, pattern := range auditpkg.DefaultNavSubstrings {
		if !strings.Contains(out, pattern) {
			t.Fatalf("run page is missing nav default %q", pattern)
		}
	}
	for _, pattern := range auditpkg.DefaultExcludeSubstrings {
		if !strings.Contains(out, pattern) {
			t.Fatalf("run page is missing excluded default %q", pattern)
		}
	}
}

func TestInclusiveAuditFiltersKeepNav(t *testing.T) {
	t.Run("reuses strict nav and clears excludes", func(t *testing.T) {
		strict := &auditpkg.Result{
			FiltersUsed: &auditpkg.FilterOptions{
				NavSubstrings:     []string{"header", "custom-nav"},
				ExcludeSubstrings: []string{"related", "slider"},
			},
		}
		got := inclusiveAuditFilters(strict)
		if !got.Explicit {
			t.Fatal("Explicit = false, want true")
		}
		if len(got.ExcludeSubstrings) != 0 {
			t.Fatalf("ExcludeSubstrings = %v, want none", got.ExcludeSubstrings)
		}
		if strings.Join(got.NavSubstrings, ",") != "header,custom-nav" {
			t.Fatalf("NavSubstrings = %v, want strict nav list", got.NavSubstrings)
		}
	})

	t.Run("preserves an explicitly empty nav list", func(t *testing.T) {
		strict := &auditpkg.Result{FiltersUsed: &auditpkg.FilterOptions{Explicit: true}}
		got := inclusiveAuditFilters(strict)
		if len(got.NavSubstrings) != 0 {
			t.Fatalf("NavSubstrings = %v, want empty", got.NavSubstrings)
		}
	})

	t.Run("falls back to defaults without a strict result", func(t *testing.T) {
		got := inclusiveAuditFilters(nil)
		if len(got.NavSubstrings) != len(auditpkg.DefaultNavSubstrings) {
			t.Fatalf("NavSubstrings = %v, want defaults", got.NavSubstrings)
		}
	})
}

func TestJobFragmentShowsAuditTabs(t *testing.T) {
	var buf bytes.Buffer
	writeJobFragment(&buf, jobView{
		ID:                    "abc123",
		Mode:                  "spider",
		Status:                "done",
		Pages:                 3,
		Audit:                 true,
		AuditReady:            true,
		AuditPages:            3,
		AuditLinks:            2,
		AuditOrphans:          0,
		AuditInclusiveReady:   true,
		AuditInclusivePages:   3,
		AuditInclusiveLinks:   5,
		AuditInclusiveOrphans: 1,
	})
	out := buf.String()
	for _, want := range []string{
		`>Strict<`,
		`>Inclusive<`,
		`/api/crawl/abc123/audit/bundle.zip`,
		`/api/crawl/abc123/audit/inclusive/bundle.zip`,
		`5 content link occurrences`,
		`1 orphan`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("job fragment is missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{
		`audit.json`,
		`audit-pages.csv`,
		`audit-links.csv`,
		`audit-blocks.csv`,
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("job fragment still exposes individual file %q:\n%s", unwanted, out)
		}
	}
}

func TestJobFragmentShowsInclusiveAuditError(t *testing.T) {
	var buf bytes.Buffer
	writeJobFragment(&buf, jobView{
		ID:                  "abc123",
		Mode:                "spider",
		Status:              "done",
		Pages:               3,
		Audit:               true,
		AuditReady:          true,
		AuditPages:          3,
		AuditLinks:          2,
		AuditInclusiveError: "inclusive failed",
	})
	if out := buf.String(); !strings.Contains(out, "inclusive failed") {
		t.Fatalf("inclusive error missing from job fragment:\n%s", out)
	}
}

func TestServeAuditBundleProfiles(t *testing.T) {
	writeAuditFixture := func(t *testing.T, marker string) string {
		t.Helper()
		dir := t.TempDir()
		files := map[string]string{
			"audit.json": `{"version":1,"start_url":"https://example.com/","domain":"example.com",` +
				`"generated_at":"2026-01-02T03:04:05Z","summary":{"pages":1,"content_page_links":1}}`,
			"audit-pages.csv":  "URL,Depth,Status\nexample.com/" + marker + ",0,200\n",
			"audit-links.csv":  "Source,Target,Decision\nexample.com/,example.com/" + marker + ",content\n",
			"audit-blocks.csv": "Block ID,Tag,Decision\nb1,main,content\n",
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	job := &crawlJob{
		auditReady:          true,
		auditDir:            writeAuditFixture(t, "strict"),
		auditInclusiveReady: true,
		auditInclusiveDir:   writeAuditFixture(t, "inclusive"),
		settings:            WizardSettings{Mode: 1, StartURL: "https://example.com/"},
	}

	for _, profile := range []string{"strict", "inclusive"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		serveAuditBundle(rec, req, job, profile)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200: %s", profile, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/zip" {
			t.Fatalf("%s content type = %q, want application/zip", profile, got)
		}
		if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, profile+"-audit.zip") {
			t.Fatalf("%s content disposition = %q", profile, got)
		}

		reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
		if err != nil {
			t.Fatalf("%s bundle is not a zip: %v", profile, err)
		}
		names := make(map[string]bool)
		var pages string
		for _, f := range reader.File {
			names[f.Name] = true
			if f.Name == "audit-pages.csv" {
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					t.Fatal(err)
				}
				pages = string(data)
			}
		}
		for _, want := range []string{"audit.xlsx", "audit.json", "audit-pages.csv", "audit-links.csv", "audit-blocks.csv", "manifest.txt"} {
			if !names[want] {
				t.Fatalf("%s bundle missing %s: %v", profile, want, names)
			}
		}
		if !strings.Contains(pages, profile) {
			t.Fatalf("%s bundle came from the wrong profile: %q", profile, pages)
		}
	}
}

// TestUIJobFlowUnchanged covers the regression contract for the shared core:
// the web UI still creates a browser-only job with its original ID shape,
// renders the job fragment, and polls it through /api/crawl/{id}.
func TestUIJobFlowUnchanged(t *testing.T) {
	runner := newFakeCrawlRunner()
	runner.pages = []Page{{URL: "https://example.com/", StatusCode: 200}}
	t.Cleanup(runner.Release)

	app, err := newApp(appConfig{MCP: defaultMCPConfig(), Runner: runner})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Close(context.Background())

	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	for key, value := range map[string]string{
		"mode":        "spider",
		"startUrl":    "https://example.com",
		"keywords":    "pricing",
		"concurrency": "2",
	} {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatalf("write form field %s: %v", key, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart form: %v", err)
	}

	resp, err := http.Post(ts.URL+"/api/crawl", mw.FormDataContentType(), &form)
	if err != nil {
		t.Fatalf("POST /api/crawl: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/crawl status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read fragment: %v", err)
	}
	fragment := string(body)
	if !strings.Contains(fragment, `class="job"`) {
		t.Fatalf("response is not a job fragment:\n%s", fragment)
	}

	match := regexp.MustCompile(`id="job-([0-9a-f]+)"`).FindStringSubmatch(fragment)
	if len(match) != 2 {
		t.Fatalf("fragment does not expose a job ID:\n%s", fragment)
	}
	jobID := match[1]
	if len(jobID) != 12 {
		t.Fatalf("UI job ID = %q, want the existing 12-character shape", jobID)
	}

	job := app.jobs.get(jobID)
	if job == nil {
		t.Fatalf("UI job %q is not in the manager", jobID)
	}
	if job.owner != ownerUI {
		t.Fatalf("UI job owner = %q, want %q", job.owner, ownerUI)
	}

	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("crawl runner was not launched")
	}

	poll, err := http.Get(ts.URL + "/api/crawl/" + jobID)
	if err != nil {
		t.Fatalf("GET /api/crawl/%s: %v", jobID, err)
	}
	defer poll.Body.Close()
	pollBody, err := io.ReadAll(poll.Body)
	if err != nil {
		t.Fatalf("read poll fragment: %v", err)
	}
	pollFragment := string(pollBody)
	for _, want := range []string{`id="job-` + jobID + `"`, `hx-get="/api/crawl/` + jobID + `"`} {
		if !strings.Contains(pollFragment, want) {
			t.Fatalf("poll fragment is missing %q:\n%s", want, pollFragment)
		}
	}

	runner.Release()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, _ := job.statusPhase()
		if status == jobStatusDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UI job did not finish; status = %q", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParseAuditFilters(t *testing.T) {
	newRequest := func(values url.Values) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/crawl", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req
	}

	t.Run("explicit spider filters are parsed and normalized", func(t *testing.T) {
		req := newRequest(url.Values{
			"audit_nav_substrings":     {" Header, mega_menu\n footer "},
			"audit_exclude_substrings": {"related\nrecommend, Related"},
			"audit_filters_explicit":   {"1"},
		})
		got := parseAuditFilters(req, "spider", true)
		if got == nil {
			t.Fatal("parseAuditFilters returned nil")
		}
		if !got.Explicit {
			t.Fatal("Explicit = false, want true")
		}
		if strings.Join(got.NavSubstrings, ",") != "header,mega-menu,footer" {
			t.Fatalf("nav = %v", got.NavSubstrings)
		}
		if strings.Join(got.ExcludeSubstrings, ",") != "related,recommend" {
			t.Fatalf("exclude = %v", got.ExcludeSubstrings)
		}
	})

	t.Run("cleared explicit lists stay explicit and empty", func(t *testing.T) {
		req := newRequest(url.Values{"audit_filters_explicit": {"1"}})
		got := parseAuditFilters(req, "spider", true)
		if got == nil {
			t.Fatal("parseAuditFilters returned nil")
		}
		if !got.Explicit || len(got.NavSubstrings) != 0 || len(got.ExcludeSubstrings) != 0 {
			t.Fatalf("got %+v, want explicit empty lists", got)
		}
	})

	t.Run("old clients without the explicit marker keep defaults", func(t *testing.T) {
		req := newRequest(url.Values{
			"audit_nav_substrings":     {"custom"},
			"audit_exclude_substrings": {"custom"},
		})
		if got := parseAuditFilters(req, "spider", true); got != nil {
			t.Fatalf("parseAuditFilters = %+v, want nil", got)
		}
	})

	t.Run("audit off ignores filters", func(t *testing.T) {
		req := newRequest(url.Values{
			"audit_nav_substrings":     {"custom"},
			"audit_exclude_substrings": {"custom"},
			"audit_filters_explicit":   {"1"},
		})
		if got := parseAuditFilters(req, "spider", false); got != nil {
			t.Fatalf("parseAuditFilters = %+v, want nil", got)
		}
	})

	t.Run("list mode ignores filters", func(t *testing.T) {
		req := newRequest(url.Values{
			"audit_nav_substrings":     {"custom"},
			"audit_exclude_substrings": {"custom"},
			"audit_filters_explicit":   {"1"},
		})
		if got := parseAuditFilters(req, "list", true); got != nil {
			t.Fatalf("parseAuditFilters = %+v, want nil", got)
		}
	})
}
