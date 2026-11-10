package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	auditpkg "go-frog/internal/audit"
)

// jobStoreSchemaVersion is written into every job.json. Bump it when the
// persisted shape changes so old directories can be rejected deliberately
// instead of being decoded with the wrong assumptions.
const jobStoreSchemaVersion = 1

// jobStore persists one directory per MCP job below <results>/jobs/<job_id>/.
// Page rows live in pages.json.gz; artifacts and audit directories are added by
// later slices. Callers never see these paths in MCP responses.
type jobStore struct {
	root    string
	jobsDir string
}

// jobStoreRecord is the versioned job.json payload.
type jobStoreRecord struct {
	Version int    `json:"version"`
	JobID   string `json:"job_id"`
	Mode    string `json:"mode"`
	Target  string `json:"target"`
	Owner   string `json:"owner"`

	Status     string    `json:"status"`
	Phase      string    `json:"phase"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	Pages       int    `json:"pages"`
	CurrentURL  string `json:"current_url,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	LimitReason string `json:"limit_reason,omitempty"`

	Limits jobStoreLimits `json:"limits"`

	ArchiveHTML bool `json:"archive_html,omitempty"`

	AuditEnabled          bool                  `json:"audit_enabled,omitempty"`
	AuditStarted          bool                  `json:"audit_started,omitempty"`
	AuditReady            bool                  `json:"audit_ready,omitempty"`
	AuditError            string                `json:"audit_error,omitempty"`
	AuditSummary          auditpkg.AuditSummary `json:"audit_summary,omitempty"`
	AuditDir              string                `json:"audit_dir,omitempty"`
	AuditInclusiveReady   bool                  `json:"audit_inclusive_ready,omitempty"`
	AuditInclusiveError   string                `json:"audit_inclusive_error,omitempty"`
	AuditInclusiveSummary auditpkg.AuditSummary `json:"audit_inclusive_summary,omitempty"`
	AuditInclusiveDir     string                `json:"audit_inclusive_dir,omitempty"`

	Error           string `json:"error,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	ClientRequestID string `json:"client_request_id,omitempty"`

	Settings jobStoreSettings `json:"settings"`
}

// jobStoreLimits freezes the effective per-job caps, keeping the duration as
// nanoseconds so it round-trips without formatting loss.
type jobStoreLimits struct {
	MaxPages        int   `json:"max_pages"`
	MaxDepth        int   `json:"max_depth"`
	MaxDurationNano int64 `json:"max_duration_nanos"`
}

// jobStoreSettings mirrors the WizardSettings fields needed to rebuild a job.
type jobStoreSettings struct {
	Mode            int                     `json:"mode"`
	StartURL        string                  `json:"start_url,omitempty"`
	CSVPath         string                  `json:"csv_path,omitempty"`
	KeywordsRaw     string                  `json:"keywords_raw,omitempty"`
	Concurrency     int                     `json:"concurrency,omitempty"`
	ProxyURL        string                  `json:"proxy_url,omitempty"`
	Audit           bool                    `json:"audit,omitempty"`
	ArchiveHTML     bool                    `json:"archive_html,omitempty"`
	AuditArchiveDir string                  `json:"audit_archive_dir,omitempty"`
	AuditFilters    *auditpkg.FilterOptions `json:"audit_filters,omitempty"`
	ListURLs        []string                `json:"list_urls,omitempty"`
}

// newJobStore creates the jobs directory and proves it is writable. A failure
// here is a startup error: running without persistence would silently violate
// the retention contract.
func newJobStore(resultsDir string) (*jobStore, error) {
	if strings.TrimSpace(resultsDir) == "" {
		return nil, errors.New("results directory must not be empty")
	}
	jobsDir := filepath.Join(resultsDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create results directory %q: %w", resultsDir, err)
	}
	probe, err := os.CreateTemp(jobsDir, ".write-probe-*")
	if err != nil {
		return nil, fmt.Errorf("results directory %q is not writable: %w", resultsDir, err)
	}
	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)
	return &jobStore{root: resultsDir, jobsDir: jobsDir}, nil
}

// validJobStoreID accepts the two generated job-ID shapes (12-byte UI and
// 128-bit MCP, both lower-case hex). Everything else is rejected before a path
// is built.
func validJobStoreID(id string) bool {
	if len(id) != 12 && len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// jobDir returns the directory for a validated job ID.
func (s *jobStore) jobDir(id string) (string, error) {
	if !validJobStoreID(id) {
		return "", fmt.Errorf("invalid job id %q", id)
	}
	return filepath.Join(s.jobsDir, id), nil
}

// save writes job.json and pages.json.gz atomically. Each file is written to a
// temporary file in the destination directory, synced, closed, and renamed.
func (s *jobStore) save(job *crawlJob) error {
	if job == nil {
		return errors.New("cannot persist a nil job")
	}
	dir, err := s.jobDir(job.id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create job directory: %w", err)
	}

	record := jobStoreRecordFromJob(job)
	if err := writeJSONAtomic(filepath.Join(dir, "job.json"), record); err != nil {
		return fmt.Errorf("write job.json: %w", err)
	}
	if err := writeGzipJSONAtomic(filepath.Join(dir, "pages.json.gz"), job.pageRows()); err != nil {
		return fmt.Errorf("write pages.json.gz: %w", err)
	}
	return nil
}

// load reads one job directory, validating the directory/record ID agreement.
func (s *jobStore) load(id string) (*crawlJob, error) {
	dir, err := s.jobDir(id)
	if err != nil {
		return nil, err
	}
	record, err := readJobStoreRecord(dir)
	if err != nil {
		return nil, err
	}
	if record.JobID != id {
		return nil, fmt.Errorf("job directory %q contains record for %q", id, record.JobID)
	}
	job, err := jobFromStoreRecord(record)
	if err != nil {
		return nil, err
	}
	job.store = s
	pages, err := readJobStorePages(dir)
	if err != nil {
		return nil, err
	}
	job.result = pages
	return job, nil
}

// loadAll returns every valid, non-malformed job directory. Invalid IDs and
// undecodable records are skipped so one damaged directory cannot prevent the
// server from starting.
func (s *jobStore) loadAll() ([]*crawlJob, error) {
	entries, err := os.ReadDir(s.jobsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && validJobStoreID(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	jobs := make([]*crawlJob, 0, len(names))
	for _, id := range names {
		job, err := s.load(id)
		if err != nil {
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// delete removes a job directory. A missing directory is not an error.
func (s *jobStore) delete(id string) error {
	dir, err := s.jobDir(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete job directory: %w", err)
	}
	return nil
}

// cleanupExpired removes terminal job directories that the retention policy
// would reap. Running and queued jobs are always kept, and malformed entries
// are left alone rather than guessed at.
func (s *jobStore) cleanupExpired(now time.Time, policy retentionConfig) ([]string, error) {
	entries, err := os.ReadDir(s.jobsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	removed := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !validJobStoreID(entry.Name()) {
			continue
		}
		dir := filepath.Join(s.jobsDir, entry.Name())
		record, err := readJobStoreRecord(dir)
		if err != nil || record.JobID != entry.Name() {
			continue
		}
		snapshot := jobRetentionSnapshot{
			id:         record.JobID,
			owner:      record.Owner,
			status:     record.Status,
			createdAt:  record.CreatedAt,
			finishedAt: record.FinishedAt,
		}
		if !shouldReap(snapshot, now, policy).reap {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return removed, fmt.Errorf("remove expired job %q: %w", record.JobID, err)
		}
		removed = append(removed, record.JobID)
	}
	sort.Strings(removed)
	return removed, nil
}

// jobStoreRecordFromJob snapshots the job under its mutex.
func jobStoreRecordFromJob(job *crawlJob) jobStoreRecord {
	job.mu.Lock()
	defer job.mu.Unlock()

	return jobStoreRecord{
		Version:     jobStoreSchemaVersion,
		JobID:       job.id,
		Mode:        job.mode,
		Target:      job.target,
		Owner:       job.owner,
		Status:      job.status,
		Phase:       job.phase,
		CreatedAt:   job.createdAt,
		StartedAt:   job.startedAt,
		FinishedAt:  job.finishedAt,
		Pages:       job.pages,
		CurrentURL:  job.currentURL,
		Truncated:   job.truncated,
		LimitReason: job.limitReason,
		Limits: jobStoreLimits{
			MaxPages:        job.limits.MaxPages,
			MaxDepth:        job.limits.MaxDepth,
			MaxDurationNano: int64(job.limits.MaxDuration),
		},
		ArchiveHTML:           job.archiveHTML,
		AuditEnabled:          job.settings.Audit,
		AuditStarted:          job.auditStarted,
		AuditReady:            job.auditReady,
		AuditError:            job.auditError,
		AuditSummary:          job.auditSummary,
		AuditDir:              job.auditDir,
		AuditInclusiveReady:   job.auditInclusiveReady,
		AuditInclusiveError:   job.auditInclusiveError,
		AuditInclusiveSummary: job.auditInclusiveSummary,
		AuditInclusiveDir:     job.auditInclusiveDir,
		Error:                 job.errMsg,
		ErrorCode:             job.errCode,
		ClientRequestID:       job.clientRequestID,
		Settings:              jobStoreSettingsFromWizard(job.settings),
	}
}

// jobStoreSettingsFromWizard copies a WizardSettings value, including the
// slices and filter profile, so the persisted record cannot alias live state.
func jobStoreSettingsFromWizard(settings WizardSettings) jobStoreSettings {
	out := jobStoreSettings{
		Mode:            settings.Mode,
		StartURL:        settings.StartURL,
		CSVPath:         settings.CSVPath,
		KeywordsRaw:     settings.KeywordsRaw,
		Concurrency:     settings.Concurrency,
		ProxyURL:        settings.ProxyURL,
		Audit:           settings.Audit,
		ArchiveHTML:     settings.ArchiveHTML,
		AuditArchiveDir: settings.AuditArchiveDir,
		ListURLs:        append([]string(nil), settings.listURLs...),
	}
	if settings.AuditFilters != nil {
		out.AuditFilters = &auditpkg.FilterOptions{
			NavSubstrings:     append([]string(nil), settings.AuditFilters.NavSubstrings...),
			ExcludeSubstrings: append([]string(nil), settings.AuditFilters.ExcludeSubstrings...),
			Explicit:          settings.AuditFilters.Explicit,
		}
	}
	return out
}

// jobFromStoreRecord validates a decoded record and rebuilds the in-memory job.
func jobFromStoreRecord(record jobStoreRecord) (*crawlJob, error) {
	if record.Version != jobStoreSchemaVersion {
		return nil, fmt.Errorf("unsupported job store version %d", record.Version)
	}
	if !validJobStoreID(record.JobID) {
		return nil, fmt.Errorf("invalid job id in record")
	}
	switch record.Owner {
	case ownerMCP, ownerUI:
	default:
		return nil, fmt.Errorf("invalid job owner %q", record.Owner)
	}
	switch record.Status {
	case jobStatusQueued, jobStatusRunning, jobStatusDone, jobStatusError, jobStatusCancelled:
	default:
		return nil, fmt.Errorf("invalid job status %q", record.Status)
	}

	settings := WizardSettings{
		Mode:            record.Settings.Mode,
		StartURL:        record.Settings.StartURL,
		CSVPath:         record.Settings.CSVPath,
		KeywordsRaw:     record.Settings.KeywordsRaw,
		Concurrency:     record.Settings.Concurrency,
		ProxyURL:        record.Settings.ProxyURL,
		Audit:           record.Settings.Audit,
		ArchiveHTML:     record.Settings.ArchiveHTML,
		AuditArchiveDir: record.Settings.AuditArchiveDir,
		listURLs:        append([]string(nil), record.Settings.ListURLs...),
	}
	if record.Settings.AuditFilters != nil {
		settings.AuditFilters = &auditpkg.FilterOptions{
			NavSubstrings:     append([]string(nil), record.Settings.AuditFilters.NavSubstrings...),
			ExcludeSubstrings: append([]string(nil), record.Settings.AuditFilters.ExcludeSubstrings...),
			Explicit:          record.Settings.AuditFilters.Explicit,
		}
	}

	return &crawlJob{
		id:              record.JobID,
		mode:            record.Mode,
		target:          record.Target,
		createdAt:       record.CreatedAt,
		owner:           record.Owner,
		limits:          jobLimits{MaxPages: record.Limits.MaxPages, MaxDepth: record.Limits.MaxDepth, MaxDuration: time.Duration(record.Limits.MaxDurationNano)},
		archiveHTML:     record.ArchiveHTML,
		clientRequestID: record.ClientRequestID,
		status:          record.Status,
		phase:           record.Phase,
		startedAt:       record.StartedAt,
		finishedAt:      record.FinishedAt,
		pages:           record.Pages,
		currentURL:      record.CurrentURL,
		errMsg:          record.Error,
		errCode:         record.ErrorCode,
		settings:        settings,
		truncated:       record.Truncated,
		limitReason:     record.LimitReason,
		auditStarted:    record.AuditStarted,
		auditReady:      record.AuditReady,
		auditError:      record.AuditError,
		auditSummary:    record.AuditSummary,
		auditDir:        record.AuditDir,

		auditInclusiveReady:   record.AuditInclusiveReady,
		auditInclusiveError:   record.AuditInclusiveError,
		auditInclusiveSummary: record.AuditInclusiveSummary,
		auditInclusiveDir:     record.AuditInclusiveDir,
	}, nil
}

// pageRows snapshots the page result under the job mutex.
func (j *crawlJob) pageRows() []Page {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Page(nil), j.result...)
}

func readJobStoreRecord(dir string) (jobStoreRecord, error) {
	var record jobStoreRecord
	data, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		return record, fmt.Errorf("read job.json: %w", err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("decode job.json: %w", err)
	}
	return record, nil
}

// readJobStorePages loads pages.json.gz. A missing file is not an error so
// early or interrupted jobs remain readable.
func readJobStorePages(dir string) ([]Page, error) {
	file, err := os.Open(filepath.Join(dir, "pages.json.gz"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open pages.json.gz: %w", err)
	}
	defer file.Close()

	reader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open gzip pages: %w", err)
	}
	defer reader.Close()

	var pages []Page
	if err := json.NewDecoder(reader).Decode(&pages); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode pages.json.gz: %w", err)
	}
	return pages, nil
}

// writeFileAtomic writes through a temp file in the same directory and renames
// it into place. A failed write never leaves the target half-updated.
func writeFileAtomic(path string, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		_ = tmp.Close()
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := write(tmp); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

// writeJSONAtomic encodes v as indented JSON through writeFileAtomic.
func writeJSONAtomic(path string, v any) error {
	return writeFileAtomic(path, func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(v)
	})
}

// writeGzipJSONAtomic encodes v as gzipped JSON through writeFileAtomic.
func writeGzipJSONAtomic(path string, v any) error {
	return writeFileAtomic(path, func(w io.Writer) error {
		zw := gzip.NewWriter(w)
		if err := json.NewEncoder(zw).Encode(v); err != nil {
			_ = zw.Close()
			return err
		}
		return zw.Close()
	})
}
