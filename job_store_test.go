package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditpkg "go-frog/internal/audit"
)

// newTestJobStore builds a store rooted in a fresh temporary directory.
func newTestJobStore(t *testing.T) (*jobStore, string) {
	t.Helper()
	root := t.TempDir()
	store, err := newJobStore(root)
	if err != nil {
		t.Fatalf("newJobStore: %v", err)
	}
	return store, root
}

// newTestPersistentManager builds a reaper-free manager backed by store.
func newTestPersistentManager(t *testing.T, store *jobStore, now time.Time) *jobManager {
	t.Helper()
	return newJobManager(jobManagerConfig{
		Retention:     defaultRetentionConfig(),
		Now:           func() time.Time { return now },
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DisableReaper: true,
		Store:         store,
	})
}

// markTestJobDone forces a deterministic terminal transition and persists it.
func markTestJobDone(t *testing.T, job *crawlJob, pages []Page, finishedAt time.Time) {
	t.Helper()
	job.mu.Lock()
	job.status = jobStatusDone
	job.phase = jobPhaseDone
	job.finishedAt = finishedAt
	job.pages = len(pages)
	job.result = pages
	job.mu.Unlock()
	job.persist()
}

// markTestJobRunning forces deterministic timestamps on a running job.
func markTestJobRunning(t *testing.T, job *crawlJob, createdAt, startedAt time.Time) {
	t.Helper()
	job.mu.Lock()
	job.status = jobStatusRunning
	job.phase = jobPhaseCrawling
	job.createdAt = createdAt
	job.startedAt = startedAt
	job.mu.Unlock()
	job.persist()
}

// jobStoreTestSettings is a fully populated settings value so persistence
// round-trips every field needed to rebuild the job.
func jobStoreTestSettings() WizardSettings {
	return WizardSettings{
		Mode:            1,
		StartURL:        "https://example.com",
		KeywordsRaw:     "go-frog|persistence",
		Concurrency:     6,
		Audit:           true,
		AuditArchiveDir: "/tmp/does-not-matter/archive",
		AuditFilters: &auditpkg.FilterOptions{
			NavSubstrings:     []string{"nav", "menu"},
			ExcludeSubstrings: []string{"related"},
			Explicit:          true,
		},
	}
}

func TestJobStore(t *testing.T) {
	t.Run("completed job and page rows survive restart", func(t *testing.T) {
		store, root := newTestJobStore(t)
		jm := newTestPersistentManager(t, store, time.Now().UTC())

		job := jm.createWithOwner(ownerMCP, "spider", "https://example.com", jobStoreTestSettings())
		pages := []Page{
			{
				URL:         "https://example.com/",
				StatusCode:  200,
				Title:       "Home",
				Description: "Example home",
				H1s:         []string{"Welcome"},
				ImgAlts:     "logo>Example",
				Robots:      "index,follow",
				KeywordHits: []int{2, 1},
			},
			{
				URL:         "https://example.com/about",
				StatusCode:  200,
				Title:       "About",
				KeywordHits: []int{0, 3},
			},
		}
		markTestJobDone(t, job, pages, time.Now().UTC())

		jobDir := filepath.Join(root, "jobs", job.id)
		for _, name := range []string{"job.json", "pages.json.gz"} {
			if _, err := os.Stat(filepath.Join(jobDir, name)); err != nil {
				t.Fatalf("missing %s after save: %v", name, err)
			}
		}
		entries, err := os.ReadDir(jobDir)
		if err != nil {
			t.Fatalf("read job dir: %v", err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), ".tmp") {
				t.Fatalf("temporary file %q left behind after a successful write", entry.Name())
			}
		}

		// A brand-new manager over the same directory simulates a restart.
		restarted := newTestPersistentManager(t, store, time.Now().UTC())
		loaded := restarted.get(job.id)
		if loaded == nil {
			t.Fatalf("job %q was not hydrated after restart", job.id)
		}

		loaded.mu.Lock()
		gotStatus, gotPhase, gotOwner := loaded.status, loaded.phase, loaded.owner
		gotPages := append([]Page(nil), loaded.result...)
		gotSettings := loaded.settings
		gotKeywords := loaded.settings.KeywordsRaw
		loaded.mu.Unlock()

		if gotStatus != jobStatusDone || gotPhase != jobPhaseDone {
			t.Fatalf("status/phase = %s/%s, want done/done", gotStatus, gotPhase)
		}
		if gotOwner != ownerMCP {
			t.Fatalf("owner = %q, want %q", gotOwner, ownerMCP)
		}
		if len(gotPages) != len(pages) {
			t.Fatalf("loaded %d page rows, want %d", len(gotPages), len(pages))
		}
		if gotPages[0].Title != "Home" || gotPages[1].Title != "About" {
			t.Fatalf("loaded page titles = %q, %q", gotPages[0].Title, gotPages[1].Title)
		}
		if len(gotPages[1].KeywordHits) != 2 || gotPages[1].KeywordHits[1] != 3 {
			t.Fatalf("loaded keyword hits = %v, want [0 3]", gotPages[1].KeywordHits)
		}
		if gotKeywords != "go-frog|persistence" {
			t.Fatalf("loaded keywords = %q", gotKeywords)
		}
		if gotSettings.Mode != 1 || gotSettings.StartURL != "https://example.com" {
			t.Fatalf("loaded settings = %+v", gotSettings)
		}
		if gotSettings.AuditFilters == nil || !gotSettings.AuditFilters.Explicit {
			t.Fatalf("loaded audit filters = %+v, want explicit", gotSettings.AuditFilters)
		}
		if gotSettings.AuditArchiveDir != "/tmp/does-not-matter/archive" {
			t.Fatalf("loaded archive dir = %q", gotSettings.AuditArchiveDir)
		}

		snapshot := loaded.statusSnapshot()
		if snapshot.JobID != job.id || snapshot.Status != jobStatusDone || snapshot.PagesCrawled != len(pages) {
			t.Fatalf("status snapshot = %+v", snapshot)
		}
		if len(restarted.list(jobFilter{Owner: ownerMCP})) != 1 {
			t.Fatalf("list_jobs view does not contain the restored job")
		}

		direct, err := store.load(job.id)
		if err != nil {
			t.Fatalf("store.load: %v", err)
		}
		if direct.id != job.id || len(direct.result) != len(pages) {
			t.Fatalf("store.load returned %+v with %d pages", direct, len(direct.result))
		}
	})

	t.Run("previously running job becomes error interrupted", func(t *testing.T) {
		store, root := newTestJobStore(t)
		base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
		jm := newTestPersistentManager(t, store, base)

		job := jm.createWithOwner(ownerMCP, "list", "2 URLs", WizardSettings{
			Mode:        2,
			KeywordsRaw: "alpha",
			Concurrency: 4,
			listURLs:    []string{"https://example.com/a", "https://example.com/b"},
		})

		// Creation itself must hit disk so a crash before the first phase
		// transition is recoverable.
		early, err := os.ReadFile(filepath.Join(root, "jobs", job.id, "job.json"))
		if err != nil {
			t.Fatalf("job was not persisted at creation: %v", err)
		}
		var earlyRecord map[string]any
		if err := json.Unmarshal(early, &earlyRecord); err != nil {
			t.Fatalf("decode creation record: %v", err)
		}
		if earlyRecord["status"] != jobStatusRunning {
			t.Fatalf("creation status = %v, want %q", earlyRecord["status"], jobStatusRunning)
		}

		markTestJobRunning(t, job, base.Add(-2*time.Hour), base.Add(-2*time.Hour))

		restarted := newTestPersistentManager(t, store, base)
		loaded := restarted.get(job.id)
		if loaded == nil {
			t.Fatalf("running job %q was not recovered", job.id)
		}
		loaded.mu.Lock()
		status, phase, code, msg := loaded.status, loaded.phase, loaded.errCode, loaded.errMsg
		finishedAt := loaded.finishedAt
		loaded.mu.Unlock()

		if status != jobStatusError || phase != jobPhaseError {
			t.Fatalf("recovered status/phase = %s/%s, want error/error", status, phase)
		}
		if code != "interrupted" {
			t.Fatalf("recovered error code = %q, want %q", code, "interrupted")
		}
		if msg == "" {
			t.Fatal("recovered job has no error message")
		}
		if finishedAt.IsZero() {
			t.Fatal("recovered job has no finished_at")
		}

		// The interrupted transition must be written back to disk.
		raw, err := os.ReadFile(filepath.Join(root, "jobs", job.id, "job.json"))
		if err != nil {
			t.Fatalf("read persisted job: %v", err)
		}
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("decode persisted job: %v", err)
		}
		if record["status"] != jobStatusError {
			t.Fatalf("persisted status = %v, want %q", record["status"], jobStatusError)
		}
		if record["error_code"] != "interrupted" {
			t.Fatalf("persisted error_code = %v, want interrupted", record["error_code"])
		}

		// A second restart keeps the already-terminal interrupted state.
		third := newTestPersistentManager(t, store, base.Add(time.Hour))
		again := third.get(job.id)
		if again == nil {
			t.Fatalf("interrupted job disappeared on the second restart")
		}
		again.mu.Lock()
		status2, code2, finished2 := again.status, again.errCode, again.finishedAt
		again.mu.Unlock()
		if status2 != jobStatusError || code2 != "interrupted" {
			t.Fatalf("second restart status/code = %s/%q", status2, code2)
		}
		if !finished2.Equal(finishedAt) {
			t.Fatalf("second restart changed finished_at from %v to %v", finishedAt, finished2)
		}
	})

	t.Run("malformed and traversing entries are rejected safely", func(t *testing.T) {
		store, root := newTestJobStore(t)
		jm := newTestPersistentManager(t, store, time.Now().UTC())

		valid := jm.createWithOwner(ownerMCP, "spider", "https://example.com", WizardSettings{Mode: 1, StartURL: "https://example.com"})
		markTestJobDone(t, valid, []Page{{URL: "https://example.com/"}}, time.Now().UTC())

		jobsDir := filepath.Join(root, "jobs")
		badJSONID := strings.Repeat("b", 32)
		if err := os.MkdirAll(filepath.Join(jobsDir, badJSONID), 0o755); err != nil {
			t.Fatalf("mkdir bad entry: %v", err)
		}
		if err := os.WriteFile(filepath.Join(jobsDir, badJSONID, "job.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("write bad json: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(jobsDir, "not-a-job-id"), 0o755); err != nil {
			t.Fatalf("mkdir invalid id: %v", err)
		}
		mismatchID := strings.Repeat("c", 32)
		mismatchDir := filepath.Join(jobsDir, mismatchID)
		if err := os.MkdirAll(mismatchDir, 0o755); err != nil {
			t.Fatalf("mkdir mismatch entry: %v", err)
		}
		mismatchRecord := `{"version":1,"job_id":"` + strings.Repeat("d", 32) + `","mode":"spider","owner":"mcp","status":"done"}`
		if err := os.WriteFile(filepath.Join(mismatchDir, "job.json"), []byte(mismatchRecord), 0o644); err != nil {
			t.Fatalf("write mismatch job: %v", err)
		}

		restarted := newTestPersistentManager(t, store, time.Now().UTC())
		if restarted.get(valid.id) == nil {
			t.Fatalf("valid job was not loaded alongside malformed entries")
		}
		if restarted.get(badJSONID) != nil || restarted.get(mismatchID) != nil {
			t.Fatalf("malformed entries were loaded")
		}
		all, err := store.loadAll()
		if err != nil {
			t.Fatalf("loadAll: %v", err)
		}
		if len(all) != 1 || all[0].id != valid.id {
			t.Fatalf("loadAll returned %d jobs, want only %q", len(all), valid.id)
		}

		// Job IDs are validated before any path is built.
		for _, id := range []string{"", "..", "../escape", "../" + valid.id, "ABC", strings.Repeat("e", 31)} {
			if _, err := store.load(id); err == nil {
				t.Fatalf("store.load(%q) succeeded, want an invalid-ID error", id)
			}
			if _, err := store.jobDir(id); err == nil {
				t.Fatalf("store.jobDir(%q) succeeded, want an invalid-ID error", id)
			}
		}
		if err := store.delete("../escape"); err == nil {
			t.Fatalf("store.delete accepted a traversing ID")
		}
	})

	t.Run("delete removes the job directory", func(t *testing.T) {
		store, root := newTestJobStore(t)
		jm := newTestPersistentManager(t, store, time.Now().UTC())

		job := jm.createWithOwner(ownerMCP, "spider", "https://example.com", WizardSettings{Mode: 1, StartURL: "https://example.com"})
		markTestJobDone(t, job, []Page{{URL: "https://example.com/"}}, time.Now().UTC())
		jobDir := filepath.Join(root, "jobs", job.id)
		if _, err := os.Stat(jobDir); err != nil {
			t.Fatalf("job dir missing before delete: %v", err)
		}

		if !jm.delete(job.id) {
			t.Fatalf("delete reported the job did not exist")
		}
		if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
			t.Fatalf("job dir still exists after delete: %v", err)
		}
		if _, err := store.load(job.id); err == nil {
			t.Fatalf("deleted job still loads")
		}
		if jm.get(job.id) != nil {
			t.Fatalf("deleted job is still in memory")
		}
	})

	t.Run("expired completed jobs are cleaned and running jobs are kept", func(t *testing.T) {
		store, root := newTestJobStore(t)
		base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
		jm := newTestPersistentManager(t, store, base)

		expired := jm.createWithOwner(ownerMCP, "spider", "https://expired.example", WizardSettings{Mode: 1, StartURL: "https://expired.example"})
		markTestJobDone(t, expired, []Page{{URL: "https://expired.example/"}}, base.Add(-25*time.Hour))
		recent := jm.createWithOwner(ownerMCP, "spider", "https://recent.example", WizardSettings{Mode: 1, StartURL: "https://recent.example"})
		markTestJobDone(t, recent, []Page{{URL: "https://recent.example/"}}, base.Add(-1*time.Hour))
		running := jm.createWithOwner(ownerMCP, "spider", "https://running.example", WizardSettings{Mode: 1, StartURL: "https://running.example"})
		markTestJobRunning(t, running, base.Add(-72*time.Hour), base.Add(-72*time.Hour))

		removed, err := store.cleanupExpired(base, defaultRetentionConfig())
		if err != nil {
			t.Fatalf("cleanupExpired: %v", err)
		}
		if len(removed) != 1 || removed[0] != expired.id {
			t.Fatalf("cleanupExpired removed %v, want [%s]", removed, expired.id)
		}
		for id, want := range map[string]bool{expired.id: false, recent.id: true, running.id: true} {
			_, statErr := os.Stat(filepath.Join(root, "jobs", id))
			if exists := statErr == nil; exists != want {
				t.Fatalf("job %s dir exists = %v, want %v", id, exists, want)
			}
		}

		// The manager reaper uses the same store cleanup for expired jobs.
		late := jm.createWithOwner(ownerMCP, "spider", "https://late.example", WizardSettings{Mode: 1, StartURL: "https://late.example"})
		markTestJobDone(t, late, []Page{{URL: "https://late.example/"}}, base.Add(-25*time.Hour))
		jm.reapAt(base)
		if _, err := os.Stat(filepath.Join(root, "jobs", late.id)); !os.IsNotExist(err) {
			t.Fatalf("reaper kept the expired job directory: %v", err)
		}
		if jm.get(late.id) != nil {
			t.Fatalf("reaper kept the expired job in memory")
		}
		if _, err := os.Stat(filepath.Join(root, "jobs", recent.id)); err != nil {
			t.Fatalf("reaper removed a retained job directory: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "jobs", running.id)); err != nil {
			t.Fatalf("reaper removed a running job directory: %v", err)
		}
		if jm.get(recent.id) == nil || jm.get(running.id) == nil {
			t.Fatalf("reaper removed a retained job from memory")
		}
	})

	t.Run("UI jobs stay process-local by default", func(t *testing.T) {
		store, root := newTestJobStore(t)
		jm := newTestPersistentManager(t, store, time.Now().UTC())

		job := jm.createWithOwner(ownerUI, "spider", "https://example.com", WizardSettings{Mode: 1, StartURL: "https://example.com"})
		markTestJobDone(t, job, []Page{{URL: "https://example.com/"}}, time.Now().UTC())

		if _, err := os.Stat(filepath.Join(root, "jobs", job.id)); !os.IsNotExist(err) {
			t.Fatalf("UI job was persisted: %v", err)
		}
		restarted := newTestPersistentManager(t, store, time.Now().UTC())
		if restarted.get(job.id) != nil {
			t.Fatalf("UI job survived a restart without persistence enabled for UI jobs")
		}
	})

	t.Run("unwritable results directory fails startup", func(t *testing.T) {
		root := t.TempDir()
		blocker := filepath.Join(root, "results-file")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("write blocker: %v", err)
		}
		if _, err := newJobStore(blocker); err == nil {
			t.Fatal("newJobStore accepted a non-directory results path")
		}

		cfg := defaultMCPConfig()
		cfg.PersistJobs = true
		cfg.ResultsDir = blocker
		if _, err := newApp(appConfig{MCP: cfg}); err == nil {
			t.Fatal("newApp started with an unwritable results directory and persistence enabled")
		}
	})

	t.Run("restored jobs are visible through get_job and list_jobs", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		cfg := defaultMCPConfig()
		cfg.PersistJobs = true
		cfg.ResultsDir = t.TempDir()

		first, _ := newMCPTestSession(t, ctx, cfg, newImmediateFakeRunner(nil, nil))
		job := first.jobs.createWithOwner(ownerMCP, "spider", "https://example.com", jobStoreTestSettings())
		markTestJobDone(t, job, []Page{{URL: "https://example.com/", Title: "Home"}}, time.Now().UTC())

		// Rebuild the app over the same results directory; no shared memory.
		second, session := newMCPTestSession(t, ctx, cfg, newImmediateFakeRunner(nil, nil))
		if second.jobs.get(job.id) == nil {
			t.Fatalf("restarted app did not hydrate job %q", job.id)
		}

		out := callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": job.id})
		if out["job_id"] != job.id || out["status"] != jobStatusDone {
			t.Fatalf("get_job after restart = %v", out)
		}
		if out["pages_crawled"] != float64(1) {
			t.Fatalf("get_job pages_crawled = %v, want 1", out["pages_crawled"])
		}

		list := callToolObject(t, ctx, session, "list_jobs", map[string]any{})
		jobs, ok := list["jobs"].([]any)
		if !ok || len(jobs) != 1 {
			t.Fatalf("list_jobs after restart = %v", list)
		}
		firstJob, ok := jobs[0].(map[string]any)
		if !ok || firstJob["job_id"] != job.id {
			t.Fatalf("list_jobs first job = %v, want %q", jobs[0], job.id)
		}
	})
}
