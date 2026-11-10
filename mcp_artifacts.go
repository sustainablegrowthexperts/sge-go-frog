package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go-frog/internal/auditbundle"
)

// csvArtifactName is the on-disk name of the request-time CSV artifact. The
// public filename keeps the existing timestamp/slug convention.
const csvArtifactName = "report.csv"

// artifactJobSnapshot is one consistent copy of the job fields the artifact
// builder needs. It is taken under the job mutex so status, pages, and audit
// readiness cannot be mixed from different moments.
type artifactJobSnapshot struct {
	status         string
	settings       WizardSettings
	pages          []Page
	createdAt      time.Time
	finishedAt     time.Time
	auditEnabled   bool
	strictReady    bool
	strictDir      string
	inclusiveReady bool
	inclusiveDir   string
}

func (j *crawlJob) artifactSnapshot() artifactJobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return artifactJobSnapshot{
		status:         j.status,
		settings:       j.settings,
		pages:          append([]Page(nil), j.result...),
		createdAt:      j.createdAt,
		finishedAt:     j.finishedAt,
		auditEnabled:   j.settings.Audit,
		strictReady:    j.auditReady,
		strictDir:      j.auditDir,
		inclusiveReady: j.auditInclusiveReady,
		inclusiveDir:   j.auditInclusiveDir,
	}
}

// artifactDescriptor is the resolved on-disk artifact plus the metadata that
// describes it to the client. path is never returned in a tool response.
type artifactDescriptor struct {
	path        string
	filename    string
	contentType string
	url         string
	inlineable  bool
}

// auditBundleCacheEntry records the source directory version that produced a
// cached ZIP. The source directory mtime invalidates the entry when the audit
// is rewritten.
type auditBundleCacheEntry struct {
	dirModTime time.Time
	path       string
}

// auditBundleCache is process-wide. The key is job ID plus profile so two jobs
// or two profiles never contend for one slot.
var auditBundleCache = struct {
	sync.Mutex
	entries map[string]auditBundleCacheEntry
}{entries: make(map[string]auditBundleCacheEntry)}

// validateGetArtifactInput checks the job ID and kind before any job lookup.
// Path-like input is rejected as invalid_argument; it is never interpreted as
// a filesystem path.
func validateGetArtifactInput(in getArtifactInput) (jobID, kind string, err error) {
	jobID = strings.TrimSpace(in.JobID)
	if jobID == "" {
		return "", "", newToolError(codeInvalidArgument, "job_id is required", nil)
	}
	if looksLikeArtifactPath(jobID) {
		return "", "", newToolError(codeInvalidArgument, "job_id must not be a path", nil)
	}

	kind = strings.TrimSpace(in.Kind)
	if kind == "" {
		return "", "", newToolError(codeInvalidArgument, "kind is required", nil)
	}
	if looksLikeArtifactPath(kind) || !slices.Contains(getArtifactKinds, kind) {
		return "", "", newToolError(codeInvalidArgument, fmt.Sprintf("kind must be one of %s", strings.Join(getArtifactKinds, ", ")), nil)
	}
	return jobID, kind, nil
}

// looksLikeArtifactPath reports whether a value could be mistaken for a
// filesystem path. Generated job IDs and artifact kinds never contain these
// characters.
func looksLikeArtifactPath(value string) bool {
	if value == "" {
		return false
	}
	if strings.ContainsRune(value, 0) || strings.ContainsAny(value, `/\`) {
		return true
	}
	if value == "." || value == ".." || strings.Contains(value, "..") {
		return true
	}
	return filepath.IsAbs(value)
}

// buildArtifact resolves and measures one artifact for a completed job. CSV is
// regenerated on every call; ZIPs are cached until the audit directory changes;
// audit JSON is read directly from the job-owned report directory.
func (s *mcpServer) buildArtifact(ctx context.Context, job *crawlJob, kind string, inline bool) (getArtifactOutput, error) {
	snapshot := job.artifactSnapshot()
	if snapshot.status != jobStatusDone {
		return getArtifactOutput{}, newToolError(
			codeJobNotFinished,
			fmt.Sprintf("job %q is %s; artifacts are available only when the job is done", job.id, snapshot.status),
			map[string]any{"job_id": job.id, "status": snapshot.status},
		)
	}

	desc, err := s.resolveArtifact(ctx, job.id, kind, snapshot)
	if err != nil {
		return getArtifactOutput{}, err
	}
	info, err := os.Stat(desc.path)
	if err != nil {
		return getArtifactOutput{}, artifactUnavailableError(kind, job.id)
	}

	out := getArtifactOutput{
		JobID:       job.id,
		Kind:        kind,
		Filename:    desc.filename,
		ContentType: desc.contentType,
		SizeBytes:   info.Size(),
		URL:         desc.url,
		ExpiresAt:   artifactExpiry(s.jobs.retention.withDefaults().MCPTTL, snapshot),
	}
	if inline && desc.inlineable && info.Size() <= s.inlineArtifactCap() {
		data, err := os.ReadFile(desc.path)
		if err != nil {
			return getArtifactOutput{}, artifactUnavailableError(kind, job.id)
		}
		// Inline content shares the tool response cap. Above it, fall back to
		// download metadata rather than returning an oversized result.
		budget := s.config.Limits.withDefaults().MaxResponseBytes
		if budget <= 0 || int64(len(data))+getArtifactResponseOverhead <= budget {
			out.Inline = true
			out.Content = string(data)
		}
	}

	attrs := []any{"kind", kind, "size_bytes", out.SizeBytes, "inline", out.Inline}
	if profile := artifactProfileForLog(kind); profile != "" {
		attrs = append(attrs, "profile", profile)
	}
	logJobEvent(s.loggerOr(), "artifact served", job.id, attrs...)
	return out, nil
}

// artifactProfileForLog maps audit artifact kinds to their report profile.
func artifactProfileForLog(kind string) string {
	switch kind {
	case "audit_strict_zip", "audit_json_strict":
		return "strict"
	case "audit_inclusive_zip", "audit_json_inclusive":
		return "inclusive"
	default:
		return ""
	}
}

// getArtifactResponseOverhead reserves envelope and metadata bytes when
// applying the tool response cap to inline content.
const getArtifactResponseOverhead = 2048

// resolveArtifact maps a whitelisted kind to its job-owned file and metadata.
// It never accepts a path from the caller, and every descriptor carries the
// same absolute job-scoped URL contract used by start_crawl.
func (s *mcpServer) resolveArtifact(ctx context.Context, jobID, kind string, snapshot artifactJobSnapshot) (artifactDescriptor, error) {
	slug := sanitizeFileSlug(resultFileSlug(snapshot.settings))
	url := absoluteArtifactURL(s.artifactBase(ctx), s.mcpPath(), jobID, kind)

	switch kind {
	case "csv":
		path := filepath.Join(s.jobOwnedDir(jobID), csvArtifactName)
		if err := writeResultsCSVAtomic(path, snapshot.pages, snapshot.settings.KeywordsRaw); err != nil {
			// Never echo the writer error: it may contain a temporary path.
			return artifactDescriptor{}, newToolError(codeInternalError, fmt.Sprintf("could not generate CSV artifact for job %q", jobID), nil)
		}
		reportTime := snapshot.finishedAt
		if reportTime.IsZero() {
			reportTime = time.Now().UTC()
		}
		return artifactDescriptor{
			path:        path,
			filename:    filepath.Base(buildResultsFilename(snapshot.settings, reportTime)),
			contentType: "text/csv",
			url:         url,
			inlineable:  true,
		}, nil

	case "audit_strict_zip", "audit_inclusive_zip":
		profile, dir, ready := auditProfileState(kind, snapshot)
		if !snapshot.auditEnabled || !ready || strings.TrimSpace(dir) == "" {
			return artifactDescriptor{}, auditNotAvailableError(kind, jobID)
		}
		path, err := s.auditBundlePath(jobID, profile, dir)
		if err != nil {
			// Never echo the builder error: it contains the audit directory.
			return artifactDescriptor{}, auditNotAvailableError(kind, jobID)
		}
		return artifactDescriptor{
			path:        path,
			filename:    slug + "-" + profile + "-audit.zip",
			contentType: "application/zip",
			url:         url,
			inlineable:  false,
		}, nil

	case "audit_json_strict", "audit_json_inclusive":
		profile, dir, ready := auditProfileState(kind, snapshot)
		if !snapshot.auditEnabled || !ready || strings.TrimSpace(dir) == "" {
			return artifactDescriptor{}, auditNotAvailableError(kind, jobID)
		}
		clean, err := cleanArtifactDirectory(dir)
		if err != nil {
			return artifactDescriptor{}, auditNotAvailableError(kind, jobID)
		}
		path := filepath.Join(clean, strictAuditFileName)
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return artifactDescriptor{}, auditNotAvailableError(kind, jobID)
		}
		return artifactDescriptor{
			path:        path,
			filename:    slug + "-" + profile + "-audit.json",
			contentType: "application/json",
			url:         url,
			inlineable:  true,
		}, nil
	}

	return artifactDescriptor{}, newToolError(codeInvalidArgument, fmt.Sprintf("kind must be one of %s", strings.Join(getArtifactKinds, ", ")), nil)
}

// auditProfileState maps a kind to its profile, directory, and readiness flag.
func auditProfileState(kind string, snapshot artifactJobSnapshot) (profile, dir string, ready bool) {
	if strings.Contains(kind, "inclusive") {
		return "inclusive", snapshot.inclusiveDir, snapshot.inclusiveReady
	}
	return "strict", snapshot.strictDir, snapshot.strictReady
}

// auditNotAvailableError is the shared audit artifact error. It deliberately
// omits the on-disk directory.
func auditNotAvailableError(kind, jobID string) error {
	return newToolError(codeAuditNotAvailable, fmt.Sprintf("%s is not available for job %q", kind, jobID), nil)
}

// artifactUnavailableError describes a missing artifact without leaking a path.
func artifactUnavailableError(kind, jobID string) error {
	if strings.HasPrefix(kind, "audit_") {
		return auditNotAvailableError(kind, jobID)
	}
	return newToolError(codeInternalError, fmt.Sprintf("%s artifact is missing for job %q", kind, jobID), nil)
}

// artifactExpiry is the job's retention completion time in RFC 3339 UTC.
func artifactExpiry(ttl time.Duration, snapshot artifactJobSnapshot) string {
	anchor := snapshot.finishedAt
	if anchor.IsZero() {
		anchor = snapshot.createdAt
	}
	if anchor.IsZero() {
		anchor = time.Now().UTC()
	}
	if ttl <= 0 {
		ttl = defaultMCPJobTTL
	}
	return anchor.UTC().Add(ttl).UTC().Format(time.RFC3339)
}

// inlineArtifactCap returns the configured inline byte cap.
func (s *mcpServer) inlineArtifactCap() int64 {
	return s.config.Limits.withDefaults().MaxInlineArtifactBytes
}

// jobOwnedDir returns the validated per-job directory below the configured
// results directory. It is never exposed to MCP clients.
func (s *mcpServer) jobOwnedDir(jobID string) string {
	resultsDir := strings.TrimSpace(s.config.ResultsDir)
	if resultsDir == "" {
		resultsDir = defaultMCPResultsDir
	}
	return filepath.Join(resultsDir, "jobs", jobID)
}

// cleanArtifactDirectory validates a job-owned directory before it is used.
// The path never comes from a client, but malformed persisted state is still
// rejected rather than followed.
func cleanArtifactDirectory(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || strings.ContainsRune(dir, 0) {
		return "", fmt.Errorf("directory is invalid")
	}
	clean := filepath.Clean(dir)
	if clean == "." || clean == string(filepath.Separator) ||
		clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("directory is invalid")
	}
	return clean, nil
}

// writeResultsCSVAtomic calls the existing writer through a temporary file in
// the destination directory and renames it into place. CSV columns stay
// exactly those produced by writeResultsCSV.
func writeResultsCSVAtomic(path string, pages []Page, keywordsRaw string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := writeResultsCSV(tmpName, pages, keywordsRaw); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

// auditBundlePath returns the cached strict/inclusive ZIP for one job. It
// rebuilds with auditbundle.WriteFile when the audit directory mtime changes
// or the cached file is missing.
func (s *mcpServer) auditBundlePath(jobID, profile, dir string) (string, error) {
	clean, err := cleanArtifactDirectory(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("audit directory is unavailable")
	}

	path := filepath.Join(s.jobOwnedDir(jobID), profile+"-audit.zip")
	key := jobID + "\x00" + profile

	auditBundleCache.Lock()
	entry, ok := auditBundleCache.entries[key]
	auditBundleCache.Unlock()
	if ok && entry.path == path && entry.dirModTime.Equal(info.ModTime()) {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := auditbundle.WriteFile(clean, path); err != nil {
		return "", err
	}

	auditBundleCache.Lock()
	auditBundleCache.entries[key] = auditBundleCacheEntry{
		dirModTime: info.ModTime(),
		path:       path,
	}
	auditBundleCache.Unlock()
	return path, nil
}

// getArtifactSummaryText renders a short agent-facing summary without a path or
// token.
func getArtifactSummaryText(out getArtifactOutput) string {
	summary := fmt.Sprintf(
		"%s artifact %s (%d bytes) for job %s.",
		out.Kind, out.Filename, out.SizeBytes, out.JobID,
	)
	if out.Inline {
		summary += " Content is included inline."
	}
	return summary
}
