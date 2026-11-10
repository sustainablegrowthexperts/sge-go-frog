package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// Download failures are split into two internal classes so the HTTP handler can
// map them to 404 or 500 while buildArtifact keeps the existing tool error
// codes. Neither error is ever returned to a client verbatim.
var (
	errArtifactUnavailable = errors.New("artifact unavailable")
	errArtifactGeneration  = errors.New("artifact generation failed")
)

// artifactUnavailable marks a missing job-scoped input (a directory, report, or
// generated cache entry) that the HTTP handler maps to 404 and the tool maps to
// its existing not-available code.
func artifactUnavailable(kind string) error {
	return fmt.Errorf("%w: %s", errArtifactUnavailable, kind)
}

// Generic HTTP bodies. They never include a path, a kind echoed from the
// request, or a builder error.
const (
	artifactNotFoundBody   = "artifact not found"
	artifactNotReadyBody   = "job is not finished"
	artifactGenerationBody = "artifact could not be generated"
)

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

// auditXLSXCacheEntry records the source directory version that produced a
// cached standalone workbook.
type auditXLSXCacheEntry struct {
	dirModTime time.Time
	path       string
}

// auditXLSXCache mirrors auditBundleCache without sharing its slots, so the ZIP
// and standalone workbook invalidate independently.
var auditXLSXCache = struct {
	sync.Mutex
	entries map[string]auditXLSXCacheEntry
}{entries: make(map[string]auditXLSXCacheEntry)}

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
		return getArtifactOutput{}, artifactToolError(err, job.id, kind)
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

// artifactToolError translates an internal resolution failure back to the
// stable get_artifact tool error codes. The download handler maps the same
// sentinels to HTTP statuses independently.
func artifactToolError(err error, jobID, kind string) error {
	switch {
	case errors.Is(err, errArtifactGeneration):
		return newToolError(codeInternalError, fmt.Sprintf("could not generate %s artifact for job %q", kind, jobID), nil)
	case errors.Is(err, errArtifactUnavailable):
		if strings.HasPrefix(kind, "audit_") {
			return auditNotAvailableError(kind, jobID)
		}
		return artifactUnavailableError(kind, jobID)
	default:
		return err
	}
}

// artifactProfileForLog maps audit artifact kinds to their report profile.
func artifactProfileForLog(kind string) string {
	switch kind {
	case "audit_strict_zip", "audit_json_strict", "audit_xlsx_strict":
		return "strict"
	case "audit_inclusive_zip", "audit_json_inclusive", "audit_xlsx_inclusive":
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
			return artifactDescriptor{}, fmt.Errorf("%w: csv", errArtifactGeneration)
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
			return artifactDescriptor{}, artifactUnavailable(kind)
		}
		path, err := s.auditBundlePath(jobID, profile, dir)
		if err != nil {
			// Never echo the builder error: it contains the audit directory.
			return artifactDescriptor{}, err
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
			return artifactDescriptor{}, artifactUnavailable(kind)
		}
		clean, err := cleanArtifactDirectory(dir)
		if err != nil {
			return artifactDescriptor{}, artifactUnavailable(kind)
		}
		path := filepath.Join(clean, strictAuditFileName)
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return artifactDescriptor{}, artifactUnavailable(kind)
		}
		return artifactDescriptor{
			path:        path,
			filename:    slug + "-" + profile + "-audit.json",
			contentType: "application/json",
			url:         url,
			inlineable:  true,
		}, nil

	case "audit_xlsx_strict", "audit_xlsx_inclusive":
		profile, dir, ready := auditProfileState(kind, snapshot)
		if !snapshot.auditEnabled || !ready || strings.TrimSpace(dir) == "" {
			return artifactDescriptor{}, artifactUnavailable(kind)
		}
		path, err := s.auditWorkbookPath(jobID, kind, profile, dir)
		if err != nil {
			// Never echo the builder error: it contains the audit directory.
			return artifactDescriptor{}, err
		}
		return artifactDescriptor{
			path:        path,
			filename:    slug + "-" + profile + "-audit.xlsx",
			contentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			url:         url,
			inlineable:  false,
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

// artifactSubtreePattern returns the mux pattern for the artifact download
// subtree. The MCP path is normalized so /mcp and /mcp/ both mount at
// /mcp/artifacts/.
func artifactSubtreePattern(mcpPath string) string {
	return normalizeMCPPath(mcpPath) + "/artifacts/"
}

// serveArtifactDownload serves one job-scoped artifact over HTTP. It shares
// resolveArtifact with get_artifact, so the HTTP and tool contracts cannot
// drift, but maps internal failures to generic HTTP statuses instead of tool
// codes.
func (s *mcpServer) serveArtifactDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		s.logArtifactDownload("", "", "", "method_not_allowed")
		return
	}

	jobID, kind, ok := s.parseArtifactDownloadPath(r)
	if !ok {
		writeArtifactHTTPError(w, http.StatusNotFound, artifactNotFoundBody)
		s.logArtifactDownload("", "", "", "not_found")
		return
	}

	job := s.jobs.get(jobID)
	if job == nil || !job.ownedBy(ownerMCP) {
		// Unknown jobs and UI-owned jobs deliberately share one response.
		writeArtifactHTTPError(w, http.StatusNotFound, artifactNotFoundBody)
		s.logArtifactDownload(jobID, kind, "", "not_found")
		return
	}

	snapshot := job.artifactSnapshot()
	if snapshot.status != jobStatusDone {
		writeArtifactHTTPError(w, http.StatusConflict, artifactNotReadyBody)
		s.logArtifactDownload(jobID, kind, snapshot.status, "not_finished")
		return
	}
	if artifactDownloadExpired(snapshot, s.jobs.retention.withDefaults().MCPTTL, s.jobs.now()) {
		writeArtifactHTTPError(w, http.StatusNotFound, artifactNotFoundBody)
		s.logArtifactDownload(jobID, kind, snapshot.status, "expired")
		return
	}

	desc, err := s.resolveArtifact(r.Context(), jobID, kind, snapshot)
	if err != nil {
		if errors.Is(err, errArtifactGeneration) {
			writeArtifactHTTPError(w, http.StatusInternalServerError, artifactGenerationBody)
			s.logArtifactDownload(jobID, kind, snapshot.status, "generation_failed")
			return
		}
		writeArtifactHTTPError(w, http.StatusNotFound, artifactNotFoundBody)
		s.logArtifactDownload(jobID, kind, snapshot.status, "not_found")
		return
	}

	file, err := os.Open(desc.path)
	if err != nil {
		writeArtifactHTTPError(w, http.StatusInternalServerError, artifactGenerationBody)
		s.logArtifactDownload(jobID, kind, snapshot.status, "read_failed")
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		writeArtifactHTTPError(w, http.StatusInternalServerError, artifactGenerationBody)
		s.logArtifactDownload(jobID, kind, snapshot.status, "read_failed")
		return
	}

	filename := sanitizeAttachmentFilename(desc.filename)
	w.Header().Set("Content-Type", desc.contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filename, info.ModTime(), file)

	s.logArtifactDownload(jobID, kind, snapshot.status, "served")
}

// parseArtifactDownloadPath extracts exactly one job ID and one whitelisted
// kind from the artifact subtree. It rejects encoded separators, extra or empty
// segments, trailing slashes, and path-like values.
func (s *mcpServer) parseArtifactDownloadPath(r *http.Request) (jobID, kind string, ok bool) {
	if r == nil || r.URL == nil {
		return "", "", false
	}

	escaped := strings.ToLower(r.URL.EscapedPath())
	for _, encoded := range []string{"%2f", "%5c", "%00"} {
		if strings.Contains(escaped, encoded) {
			return "", "", false
		}
	}

	prefix := s.mcpPath() + "/artifacts/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if strings.ContainsRune(rest, 0) || strings.ContainsRune(rest, '\\') {
		return "", "", false
	}

	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	jobID, kind = parts[0], parts[1]
	if looksLikeArtifactPath(jobID) || looksLikeArtifactPath(kind) {
		return "", "", false
	}
	if !slices.Contains(getArtifactKinds, kind) {
		return "", "", false
	}
	return jobID, kind, true
}

// artifactDownloadExpired applies the same TTL anchor as retention. A job that
// exists in memory but is past its TTL is treated as gone.
func artifactDownloadExpired(snapshot artifactJobSnapshot, ttl time.Duration, now time.Time) bool {
	if ttl <= 0 {
		ttl = defaultMCPJobTTL
	}
	anchor := snapshot.finishedAt
	if anchor.IsZero() {
		anchor = snapshot.createdAt
	}
	if anchor.IsZero() {
		return false
	}
	return !anchor.Add(ttl).After(now)
}

// sanitizeAttachmentFilename removes path components and characters that could
// break or inject a Content-Disposition header.
func sanitizeAttachmentFilename(name string) string {
	name = sanitizeFileSlug(filepath.Base(strings.TrimSpace(name)))
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == '\n' || r == '"' || r == '\\':
			return -1
		case r < 0x20 || r == 0x7f:
			return -1
		default:
			return r
		}
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "artifact"
	}
	return name
}

// writeArtifactHTTPError writes one generic, non-leaking response body.
func writeArtifactHTTPError(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, body)
}

// logArtifactDownload records only identifiers, status, and outcome. Paths and
// builder errors never enter a log line.
func (s *mcpServer) logArtifactDownload(jobID, kind, status, outcome string) {
	attrs := []any{}
	if kind != "" {
		attrs = append(attrs, "kind", kind)
	}
	if status != "" {
		attrs = append(attrs, "status", status)
	}
	attrs = append(attrs, "outcome", outcome)
	logJobEvent(s.loggerOr(), "artifact download", jobID, attrs...)
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
		return "", fmt.Errorf("%w: audit directory", errArtifactUnavailable)
	}
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: audit directory", errArtifactUnavailable)
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
		return "", fmt.Errorf("%w: bundle destination", errArtifactGeneration)
	}
	if err := auditbundle.WriteFile(clean, path); err != nil {
		return "", fmt.Errorf("%w: bundle", errArtifactGeneration)
	}

	auditBundleCache.Lock()
	auditBundleCache.entries[key] = auditBundleCacheEntry{
		dirModTime: info.ModTime(),
		path:       path,
	}
	auditBundleCache.Unlock()
	return path, nil
}

// auditWorkbookPath returns the cached standalone XLSX for one job. It rebuilds
// with auditbundle.WriteWorkbook when the audit directory mtime changes or the
// cached file is missing.
func (s *mcpServer) auditWorkbookPath(jobID, kind, profile, dir string) (string, error) {
	clean, err := cleanArtifactDirectory(dir)
	if err != nil {
		return "", fmt.Errorf("%w: audit directory", errArtifactUnavailable)
	}
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: audit directory", errArtifactUnavailable)
	}

	path := filepath.Join(s.jobOwnedDir(jobID), profile+"-audit.xlsx")
	key := jobID + "\x00" + kind + "\x00" + profile

	auditXLSXCache.Lock()
	entry, ok := auditXLSXCache.entries[key]
	auditXLSXCache.Unlock()
	if ok && entry.path == path && entry.dirModTime.Equal(info.ModTime()) {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("%w: workbook destination", errArtifactGeneration)
	}
	if err := auditbundle.WriteWorkbook(clean, path); err != nil {
		return "", fmt.Errorf("%w: workbook", errArtifactGeneration)
	}

	auditXLSXCache.Lock()
	auditXLSXCache.entries[key] = auditXLSXCacheEntry{
		dirModTime: info.ModTime(),
		path:       path,
	}
	auditXLSXCache.Unlock()
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
