package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	auditpkg "go-frog/internal/audit"
	"go-frog/internal/auditbundle"
)

//go:embed web/*
var webFS embed.FS

var indexTmpl = template.Must(template.ParseFS(webFS, "web/index.html"))

// ── job manager ──────────────────────────────────────────────────────────────

// retentionConfig is the reaper policy for finished jobs. Running and queued
// jobs are never subject to it.
type retentionConfig struct {
	MCPTTL             time.Duration
	UITTL              time.Duration
	ReapInterval       time.Duration
	MaxRetainedMCPJobs int
}

func defaultRetentionConfig() retentionConfig {
	return retentionConfig{
		MCPTTL:             defaultMCPJobTTL,
		UITTL:              defaultUIJobTTL,
		ReapInterval:       defaultJobReapInterval,
		MaxRetainedMCPJobs: defaultMaxRetainedMCPJobs,
	}
}

// withDefaults fills unset or non-positive fields from the contract defaults.
func (c retentionConfig) withDefaults() retentionConfig {
	def := defaultRetentionConfig()
	if c.MCPTTL <= 0 {
		c.MCPTTL = def.MCPTTL
	}
	if c.UITTL <= 0 {
		c.UITTL = def.UITTL
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = def.ReapInterval
	}
	if c.MaxRetainedMCPJobs <= 0 {
		c.MaxRetainedMCPJobs = def.MaxRetainedMCPJobs
	}
	return c
}

// jobManagerConfig supplies the retention policy and the clock/ticker seams
// used by tests. Production uses the zero value.
type jobManagerConfig struct {
	Retention     retentionConfig
	Now           func() time.Time
	Ticker        func(time.Duration) <-chan time.Time
	Logger        *slog.Logger
	DisableReaper bool

	// Store enables per-job filesystem persistence. MCP jobs are persisted by
	// default; UI jobs remain process-local unless PersistUIJobs is set.
	Store         *jobStore
	PersistUIJobs bool
}

type jobManager struct {
	mu   sync.RWMutex
	jobs map[string]*crawlJob

	// idempotency maps owner + "\x00" + client_request_id to the job that
	// first used the key and the canonical fingerprint of that request. It is
	// guarded by mu and purged whenever the mapped job is deleted or reaped.
	idempotency map[string]idempotencyEntry

	retention retentionConfig
	now       func() time.Time
	ticker    func(time.Duration) <-chan time.Time
	logger    *slog.Logger

	store     *jobStore
	persistUI bool
}

// idempotencyEntry records the job owning a request key and the effective
// request fingerprint stored when that job was created.
type idempotencyEntry struct {
	jobID       string
	fingerprint string
}

type crawlJob struct {
	id        string
	mode      string // "spider" | "list"
	target    string
	createdAt time.Time
	owner     string // "ui" | "mcp"

	// limits, archiveHTML, and clientRequestID are immutable after creation.
	limits          jobLimits
	archiveHTML     bool
	clientRequestID string

	// store, logger, and persistUI wire persistence hooks. A nil store keeps
	// the job process-local. persistMu serializes writes for one job so an
	// older snapshot can never overwrite a newer terminal state.
	store     *jobStore
	logger    *slog.Logger
	persistUI bool
	persistMu sync.Mutex

	mu         sync.Mutex
	status     string // "running" | "done" | "error"
	phase      string // "starting" | "crawling" | "done" | "error"
	startedAt  time.Time
	finishedAt time.Time
	pages      int
	currentURL string
	errMsg     string
	errCode    string
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

func newJobManager(cfg jobManagerConfig) *jobManager {
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	ticker := cfg.Ticker
	if ticker == nil {
		ticker = func(d time.Duration) <-chan time.Time { return time.Tick(d) }
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	jm := &jobManager{
		jobs:        make(map[string]*crawlJob),
		idempotency: make(map[string]idempotencyEntry),
		retention:   cfg.Retention.withDefaults(),
		now:         now,
		ticker:      ticker,
		logger:      logger,
		store:       cfg.Store,
		persistUI:   cfg.PersistUIJobs,
	}
	if jm.store != nil {
		jm.hydrate()
	}
	if !cfg.DisableReaper {
		go jm.reapLoop()
	}
	return jm
}

// logJobEvent writes one structured lifecycle record with the shared job_id
// field. Callers supply only identifiers, phases, and counts.
func logJobEvent(logger *slog.Logger, msg, jobID string, attrs ...any) {
	fields := make([]any, 0, len(attrs)+2)
	fields = append(fields, "job_id", jobID)
	fields = append(fields, attrs...)
	loggerOr(logger).Info(msg, fields...)
}

// logJobError writes a sanitized server-side failure record.
func logJobError(logger *slog.Logger, msg, jobID string, attrs ...any) {
	fields := make([]any, 0, len(attrs)+2)
	fields = append(fields, "job_id", jobID)
	fields = append(fields, attrs...)
	loggerOr(logger).Error(msg, fields...)
}

// hydrate restores persisted jobs, cleans expired terminal directories, and
// marks jobs that were queued or running when the previous process stopped as
// error/interrupted.
func (jm *jobManager) hydrate() {
	if _, err := jm.store.cleanupExpired(jm.now(), jm.retention); err != nil {
		jm.logger.Warn("job store: cleanup expired jobs failed", "error", err)
	}
	jobs, err := jm.store.loadAll()
	if err != nil {
		jm.logger.Warn("job store: loading persisted jobs failed", "error", err)
		return
	}

	for _, job := range jobs {
		job.store = jm.store
		job.logger = jm.logger
		job.persistUI = jm.persistUI

		job.mu.Lock()
		interrupted := job.status == jobStatusQueued || job.status == jobStatusRunning
		if interrupted {
			job.status = jobStatusError
			job.phase = jobPhaseError
			job.errCode = "interrupted"
			job.errMsg = "job interrupted by server restart"
			job.finishedAt = jm.now()
		}
		job.mu.Unlock()

		jm.jobs[job.id] = job
		if interrupted {
			jm.persistJob(job)
		}
	}
}

// persistJob writes one job to the store, respecting the UI opt-in. Persistence
// failures are logged but do not fail the request that triggered them.
func (jm *jobManager) persistJob(job *crawlJob) {
	if jm.store == nil || job == nil {
		return
	}
	job.persist()
}

// createWithOwner creates an owner-scoped job without an idempotency key.
func (jm *jobManager) createWithOwner(owner, mode, target string, s WizardSettings) *crawlJob {
	job, _, _ := jm.createWithRequest(owner, mode, target, s, jobLimits{}, false, "", "")
	return job
}

// createWithRequest creates a job and, when clientRequestID is set, atomically
// records its idempotency mapping. It returns the existing job and replay=true
// for a matching repeat, or an idempotencyConflictError when the same key was
// used with different effective input. The check and insert share one write
// lock so concurrent retries cannot create duplicate jobs.
func (jm *jobManager) createWithRequest(owner, mode, target string, s WizardSettings, limits jobLimits, archiveHTML bool, clientRequestID, fingerprint string) (*crawlJob, bool, error) {
	key := ""
	if clientRequestID != "" {
		key = idempotencyKey(owner, clientRequestID)
	}

	jm.mu.Lock()

	if key != "" {
		if entry, ok := jm.idempotency[key]; ok {
			if existing := jm.jobs[entry.jobID]; existing != nil {
				if entry.fingerprint != fingerprint {
					jm.mu.Unlock()
					return nil, false, &idempotencyConflictError{ExistingJobID: entry.jobID}
				}
				jm.mu.Unlock()
				return existing, true, nil
			}
			// The index entry outlived its job. Drop it and treat the request
			// as new; delete/reap should have removed it already.
			delete(jm.idempotency, key)
		}
	}

	var id string
	if owner == ownerMCP {
		id = newMCPJobID()
	} else {
		id = newJobID()
	}
	now := time.Now().UTC()
	j := &crawlJob{
		id:              id,
		mode:            mode,
		target:          target,
		createdAt:       now,
		owner:           owner,
		limits:          limits,
		archiveHTML:     archiveHTML,
		clientRequestID: clientRequestID,
		store:           jm.store,
		logger:          jm.logger,
		persistUI:       jm.persistUI,
		status:          jobStatusRunning,
		phase:           jobPhaseStarting,
		settings:        s,
	}
	jm.jobs[j.id] = j
	if key != "" {
		if jm.idempotency == nil {
			jm.idempotency = make(map[string]idempotencyEntry)
		}
		jm.idempotency[key] = idempotencyEntry{jobID: j.id, fingerprint: fingerprint}
	}
	jm.mu.Unlock()

	logJobEvent(jm.logger, "job created", j.id,
		"owner", owner,
		"mode", mode,
		"target_host", s.targetHost(),
		"max_pages", limits.MaxPages,
		"max_depth", limits.MaxDepth,
		"max_duration_seconds", int(limits.MaxDuration/time.Second),
	)

	// Persist the early running/queued state so a crash can be recovered.
	jm.persistJob(j)
	return j, false, nil
}

// targetHost returns only the target host for structured logs.
func (s WizardSettings) targetHost() string {
	if host := hostFromURL(s.StartURL); host != "" {
		return host
	}
	for _, raw := range s.listURLs {
		if host := hostFromURL(raw); host != "" {
			return host
		}
	}
	return ""
}

// targetHost returns only the job's target host for structured logs.
func (j *crawlJob) targetHost() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if host := j.settings.targetHost(); host != "" {
		return host
	}
	return hostFromURL(j.target)
}

// idempotencyKey scopes request IDs to one owner so different callers can use
// the same key without colliding.
func idempotencyKey(owner, clientRequestID string) string {
	return owner + "\x00" + clientRequestID
}

// crawlFingerprintPayload is the canonical, deterministic shape hashed for an
// idempotent request. Field order is fixed by the struct and list URLs are
// sorted/deduplicated so equivalent list requests share a fingerprint.
type crawlFingerprintPayload struct {
	Mode             string                        `json:"mode"`
	StartURL         string                        `json:"start_url,omitempty"`
	URLs             []string                      `json:"urls,omitempty"`
	Keywords         []string                      `json:"keywords,omitempty"`
	Concurrency      int                           `json:"concurrency"`
	Audit            bool                          `json:"audit"`
	ArchiveHTML      bool                          `json:"archive_html,omitempty"`
	AuditFilters     *crawlFingerprintAuditFilters `json:"audit_filters,omitempty"`
	MaxPages         int                           `json:"max_pages"`
	MaxDepth         int                           `json:"max_depth"`
	MaxDurationNanos int64                         `json:"max_duration_nanos"`
}

// crawlFingerprintAuditFilters is the canonical filter profile included in the
// request fingerprint only when the caller supplied one.
type crawlFingerprintAuditFilters struct {
	NavSubstrings     []string `json:"nav_substrings,omitempty"`
	ExcludeSubstrings []string `json:"exclude_substrings,omitempty"`
	Explicit          bool     `json:"explicit"`
}

// canonicalStartCrawlFingerprint returns a stable hash of the effective
// start_crawl request: mode, target URLs, keywords, concurrency, audit
// settings, audit filters, and per-job limits.
func canonicalStartCrawlFingerprint(plan startCrawlPlan) string {
	payload := crawlFingerprintPayload{
		Mode:             plan.mode,
		Keywords:         splitJobKeywords(plan.settings.KeywordsRaw),
		Concurrency:      plan.settings.Concurrency,
		Audit:            plan.settings.Audit,
		ArchiveHTML:      plan.settings.ArchiveHTML || plan.settings.Audit,
		MaxPages:         plan.limits.MaxPages,
		MaxDepth:         plan.limits.MaxDepth,
		MaxDurationNanos: int64(plan.limits.MaxDuration),
	}

	switch plan.mode {
	case "spider":
		payload.StartURL = plan.settings.StartURL
	case "list":
		urls := append([]string(nil), plan.settings.listURLs...)
		slices.Sort(urls)
		payload.URLs = slices.Compact(urls)
	}

	if filters := plan.settings.AuditFilters; filters != nil {
		payload.AuditFilters = &crawlFingerprintAuditFilters{
			NavSubstrings:     append([]string(nil), filters.NavSubstrings...),
			ExcludeSubstrings: append([]string(nil), filters.ExcludeSubstrings...),
			Explicit:          filters.Explicit,
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		// Every field is a plain JSON type, so this is unreachable; returning
		// an empty fingerprint keeps a create from being treated as a replay.
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
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

// delete removes a job from the manager, purges its idempotency mapping, and
// removes its persisted directory. It reports whether the job existed.
func (jm *jobManager) delete(id string) bool {
	jm.mu.Lock()
	ok := jm.deleteLocked(id)
	jm.mu.Unlock()

	if ok && jm.store != nil {
		if err := jm.store.delete(id); err != nil {
			jm.logger.Warn("job store: delete job failed", "job_id", id, "error", err)
		}
	}
	return ok
}

// deleteLocked removes a job and its mapping. The caller must hold jm.mu.
func (jm *jobManager) deleteLocked(id string) bool {
	job, ok := jm.jobs[id]
	if !ok {
		return false
	}
	delete(jm.jobs, id)
	jm.purgeIdempotencyLocked(job)
	return true
}

// purgeIdempotencyLocked removes the index entry owned by job. The caller must
// hold jm.mu.
func (jm *jobManager) purgeIdempotencyLocked(job *crawlJob) {
	if job == nil || job.clientRequestID == "" {
		return
	}
	key := idempotencyKey(job.owner, job.clientRequestID)
	if entry, ok := jm.idempotency[key]; ok && entry.jobID == job.id {
		delete(jm.idempotency, key)
	}
}

func (jm *jobManager) reapLoop() {
	for range jm.ticker(jm.retention.ReapInterval) {
		jm.reapAt(jm.now())
	}
}

// jobRetentionSnapshot is the consistent view of a job used to make a
// retention decision without holding the manager lock.
type jobRetentionSnapshot struct {
	id         string
	owner      string
	status     string
	createdAt  time.Time
	finishedAt time.Time
}

func (j *crawlJob) retentionSnapshot() jobRetentionSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return jobRetentionSnapshot{
		id:         j.id,
		owner:      j.owner,
		status:     j.status,
		createdAt:  j.createdAt,
		finishedAt: j.finishedAt,
	}
}

const (
	retentionReasonTTL            = "ttl"
	retentionReasonCap            = "cap"
	retentionReasonRunningSkipped = "running_skipped"
)

type retentionDecision struct {
	jobID             string
	owner             string
	status            string
	age               time.Duration
	reason            string
	reap              bool
	missingFinishedAt bool
}

// shouldReap is the pure retention policy decision for one job snapshot.
// Queued and running jobs are always retained; terminal jobs expire after the
// owner's TTL measured from finishedAt, or createdAt when finishedAt is unset.
func shouldReap(job jobRetentionSnapshot, now time.Time, policy retentionConfig) retentionDecision {
	decision := retentionDecision{
		jobID:  job.id,
		owner:  job.owner,
		status: job.status,
	}
	if job.status == jobStatusQueued || job.status == jobStatusRunning {
		decision.reason = retentionReasonRunningSkipped
		decision.age = now.Sub(job.createdAt)
		return decision
	}

	anchor := job.finishedAt
	if anchor.IsZero() {
		anchor = job.createdAt
		decision.missingFinishedAt = true
	}
	decision.age = now.Sub(anchor)

	ttl := policy.UITTL
	if job.owner == ownerMCP {
		ttl = policy.MCPTTL
	}
	if !anchor.Add(ttl).After(now) {
		decision.reason = retentionReasonTTL
		decision.reap = true
	}
	return decision
}

// retentionAnchor is the timestamp that orders finished MCP jobs for the cap.
func retentionAnchor(job jobRetentionSnapshot) time.Time {
	if job.finishedAt.IsZero() {
		return job.createdAt
	}
	return job.finishedAt
}

// reapAt applies the TTL and retained-cap policies at now. Job IDs are
// collected first and deleted afterwards so the manager lock is never held
// while future slices clean up on-disk artifacts.
func (jm *jobManager) reapAt(now time.Time) {
	jm.mu.RLock()
	jobs := make([]*crawlJob, 0, len(jm.jobs))
	for _, job := range jm.jobs {
		jobs = append(jobs, job)
	}
	jm.mu.RUnlock()

	snapshots := make([]jobRetentionSnapshot, 0, len(jobs))
	for _, job := range jobs {
		snapshots = append(snapshots, job.retentionSnapshot())
	}

	type capCandidate struct {
		decision retentionDecision
		anchor   time.Time
	}

	deleteIDs := make(map[string]struct{})
	finishedMCP := make([]capCandidate, 0, len(snapshots))
	for _, snapshot := range snapshots {
		decision := shouldReap(snapshot, now, jm.retention)
		if decision.missingFinishedAt {
			jm.logger.Warn("retention: terminal job is missing finished_at; falling back to created_at",
				"job_id", decision.jobID,
				"owner", decision.owner,
				"status", decision.status,
			)
		}
		if decision.reap {
			deleteIDs[decision.jobID] = struct{}{}
			jm.logRetentionDecision(decision)
			continue
		}
		if decision.reason == retentionReasonRunningSkipped {
			jm.logRetentionDecision(decision)
			continue
		}
		if snapshot.owner == ownerMCP {
			finishedMCP = append(finishedMCP, capCandidate{
				decision: decision,
				anchor:   retentionAnchor(snapshot),
			})
		}
	}

	if len(finishedMCP) > jm.retention.MaxRetainedMCPJobs {
		sort.Slice(finishedMCP, func(i, k int) bool {
			if finishedMCP[i].anchor.Equal(finishedMCP[k].anchor) {
				return finishedMCP[i].decision.jobID < finishedMCP[k].decision.jobID
			}
			return finishedMCP[i].anchor.Before(finishedMCP[k].anchor)
		})
		evict := len(finishedMCP) - jm.retention.MaxRetainedMCPJobs
		for i := 0; i < evict; i++ {
			decision := finishedMCP[i].decision
			decision.reason = retentionReasonCap
			decision.reap = true
			deleteIDs[decision.jobID] = struct{}{}
			jm.logRetentionDecision(decision)
		}
	}

	if len(deleteIDs) == 0 {
		return
	}
	jm.mu.Lock()
	for id := range deleteIDs {
		jm.deleteLocked(id)
	}
	jm.mu.Unlock()

	// Remove persisted directories only after the manager lock is released.
	// Running jobs never reach deleteIDs, so their directories are never
	// touched here.
	if jm.store != nil {
		for id := range deleteIDs {
			if err := jm.store.delete(id); err != nil {
				jm.logger.Warn("job store: delete reaped job failed", "job_id", id, "error", err)
			}
		}
	}
}

// logRetentionDecision emits one structured, content-free retention record.
func (jm *jobManager) logRetentionDecision(decision retentionDecision) {
	jm.logger.Info("retention decision",
		"job_id", decision.jobID,
		"owner", decision.owner,
		"status", decision.status,
		"age", decision.age.String(),
		"reason", decision.reason,
	)
}

// persist writes the current job state to the configured store. A nil store
// keeps the job process-local. UI jobs are only written when explicitly enabled.
// The per-job write lock makes every snapshot reflect the state at write time,
// so a slow earlier write cannot clobber a later terminal transition.
func (j *crawlJob) persist() {
	if j.store == nil {
		return
	}
	if j.owner != ownerMCP && !j.persistUI {
		return
	}
	j.persistMu.Lock()
	defer j.persistMu.Unlock()
	if err := j.store.save(j); err != nil && j.logger != nil {
		j.logger.Warn("job store: persist job failed", "job_id", j.id, "error", err)
	}
}

// markQueued moves a newly created job into the bounded MCP wait queue. It is
// only used for MCP jobs; the web UI keeps launching immediately.
func (j *crawlJob) markQueued() {
	j.mu.Lock()
	j.status = jobStatusQueued
	j.setPhaseLocked(jobPhaseQueued)
	j.mu.Unlock()
	j.persist()
}

// startFromQueue transitions an admitted queued job to running/starting and
// records its real start time.
func (j *crawlJob) startFromQueue() {
	j.mu.Lock()
	if j.status != jobStatusQueued {
		j.mu.Unlock()
		return
	}
	j.status = jobStatusRunning
	j.setPhaseLocked(jobPhaseStarting)
	j.mu.Unlock()
	j.persist()
}

// setPhaseLocked applies the phase lifecycle rules while the caller holds the
// job mutex. It reports whether the phase changed. startedAt is recorded the
// first time the job enters an active phase, finishedAt at a terminal phase,
// and a terminal phase never regresses to an active one.
func (j *crawlJob) setPhaseLocked(phase string) bool {
	if isTerminalPhase(j.phase) && !isTerminalPhase(phase) {
		return false
	}
	changed := j.phase != phase
	j.phase = phase
	if j.startedAt.IsZero() && isActivePhase(phase) {
		j.startedAt = time.Now().UTC()
	}
	if isTerminalPhase(phase) && j.finishedAt.IsZero() {
		j.finishedAt = time.Now().UTC()
	}
	if changed {
		logJobEvent(j.logger, "job phase", j.id, "phase", phase)
	}
	return changed
}

// setPhase moves the job to a new lifecycle phase and persists real changes.
// Progress callbacks never write.
func (j *crawlJob) setPhase(phase string) {
	j.mu.Lock()
	changed := j.setPhaseLocked(phase)
	j.mu.Unlock()
	if changed {
		j.persist()
	}
}

// beginAudit marks the start of the audit pipeline so polling can distinguish
// a pending audit from one that is running.
func (j *crawlJob) beginAudit() {
	j.mu.Lock()
	j.auditStarted = true
	j.mu.Unlock()
	j.persist()
	logJobEvent(j.logger, "audit started", j.id)
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

// terminalLocked reports whether the job already reached a terminal state. The
// caller must hold the job mutex. Terminal transitions are first-writer-wins so
// a shutdown interruption cannot be overwritten by a late crawl result.
func (j *crawlJob) terminalLocked() bool {
	switch j.status {
	case jobStatusDone, jobStatusError, jobStatusCancelled:
		return true
	}
	return false
}

func (j *crawlJob) finish(pages []Page) {
	j.mu.Lock()
	if j.terminalLocked() {
		j.mu.Unlock()
		return
	}
	j.status = jobStatusDone
	j.setPhaseLocked(jobPhaseDone)
	j.result = pages
	j.pages = len(pages)
	j.mu.Unlock()
	j.persist()
	j.logFinished()
}

// logFinished writes the terminal job record with counts and duration.
func (j *crawlJob) logFinished() {
	j.mu.Lock()
	pages := j.pages
	truncated := j.truncated
	limitReason := j.limitReason
	startedAt := j.startedAt
	finishedAt := j.finishedAt
	auditEnabled := j.settings.Audit
	auditStrict := j.auditReady
	auditInclusive := j.auditInclusiveReady
	j.mu.Unlock()

	duration := time.Duration(0)
	if !startedAt.IsZero() {
		if finishedAt.IsZero() {
			duration = time.Since(startedAt)
		} else {
			duration = finishedAt.Sub(startedAt)
		}
	} else if !j.createdAt.IsZero() {
		duration = time.Since(j.createdAt)
	}

	attrs := []any{
		"pages", pages,
		"duration_ms", duration.Milliseconds(),
		"truncated", truncated,
	}
	if limitReason != "" {
		attrs = append(attrs, "limit_reason", limitReason)
	}
	if auditEnabled {
		attrs = append(attrs, "audit_strict_ready", auditStrict, "audit_inclusive_ready", auditInclusive)
	}
	logJobEvent(j.logger, "job finished", j.id, attrs...)
}

// setTruncation records the crawl outcome's cap metadata. A non-truncated
// outcome clears any previous reason; terminal paths persist the state.
func (j *crawlJob) setTruncation(truncated bool, reason string) {
	if !truncated && reason == "" {
		return
	}
	j.mu.Lock()
	j.truncated = truncated
	j.limitReason = reason
	j.mu.Unlock()
}

func (j *crawlJob) fail(err error) {
	sanitized := j.sanitizedError(err)
	j.mu.Lock()
	if j.terminalLocked() {
		j.mu.Unlock()
		return
	}
	j.status = jobStatusError
	j.setPhaseLocked(jobPhaseError)
	j.errMsg = err.Error()
	j.mu.Unlock()
	j.persist()
	logJobError(j.logger, "job failed", j.id, "error", sanitized)
}

// interrupt records the shutdown terminal state for a queued or running job. A
// job that already finished is left untouched. Partial rows recorded before the
// interruption are preserved.
func (j *crawlJob) interrupt(message string) bool {
	if message == "" {
		message = jobInterruptedMessage
	}
	j.mu.Lock()
	if j.terminalLocked() {
		j.mu.Unlock()
		return false
	}
	j.status = jobStatusError
	j.setPhaseLocked(jobPhaseError)
	j.errMsg = message
	j.errCode = "interrupted"
	j.mu.Unlock()
	j.persist()
	logJobError(j.logger, "job interrupted", j.id, "reason", "interrupted")
	return true
}

// setPartialResults preserves page rows that were already recorded when a run
// was interrupted. It never changes the terminal status or error code.
func (j *crawlJob) setPartialResults(pages []Page) {
	if len(pages) == 0 {
		return
	}
	j.mu.Lock()
	j.result = append([]Page(nil), pages...)
	j.pages = len(pages)
	j.mu.Unlock()
	j.persist()
}

// sanitizedError removes known job-owned filesystem prefixes and credentials
// from a server-side error record.
func (j *crawlJob) sanitizedError(err error) string {
	if err == nil {
		return ""
	}
	j.mu.Lock()
	dirs := []string{j.settings.AuditArchiveDir}
	j.mu.Unlock()
	text := sanitizeDiagnosticText(err.Error())
	for _, dir := range dirs {
		if dir = strings.TrimSpace(dir); dir != "" {
			text = strings.ReplaceAll(text, dir, "<job_dir>")
		}
	}
	return text
}

func (j *crawlJob) finishWithAudits(pages []Page, strict, inclusive *auditpkg.Result, inclusiveErr error) {
	j.mu.Lock()
	if j.terminalLocked() {
		j.mu.Unlock()
		return
	}
	j.status = jobStatusDone
	j.setPhaseLocked(jobPhaseDone)
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
	j.mu.Unlock()
	j.persist()

	j.mu.Lock()
	strictPages := j.auditSummary.Pages
	inclusivePages := j.auditInclusiveSummary.Pages
	inclusiveReady := j.auditInclusiveReady
	j.mu.Unlock()
	logJobEvent(j.logger, "audit finished", j.id,
		"strict", true,
		"inclusive", inclusiveReady,
		"strict_pages", strictPages,
		"inclusive_pages", inclusivePages,
	)
	j.logFinished()
}

func (j *crawlJob) finishWithAuditError(pages []Page, err error) {
	sanitized := j.sanitizedError(err)
	j.mu.Lock()
	if j.terminalLocked() {
		j.mu.Unlock()
		return
	}
	j.status = jobStatusDone
	j.setPhaseLocked(jobPhaseDone)
	j.result = pages
	j.pages = len(pages)
	j.auditReady = false
	j.auditError = err.Error()
	j.mu.Unlock()
	j.persist()
	logJobError(j.logger, "audit failed", j.id, "error", sanitized)
	j.logFinished()
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
			CSVReady:                j.status == jobStatusDone && len(j.result) > 0,
			AuditStrictReady:        j.auditReady,
			AuditInclusiveReady:     j.auditInclusiveReady,
			AuditXLSXStrictReady:    j.auditReady,
			AuditXLSXInclusiveReady: j.auditInclusiveReady,
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
	// SSRF overrides the public-target policy. Tests use it to reach httptest
	// loopback servers; production uses the public-only default.
	SSRF *ssrfPolicy
	// Logger is injected into the shared job manager and MCP adapter. A nil
	// value selects the process logger configured by GOFROG_LOG_FORMAT.
	Logger *slog.Logger
	// ShutdownTimeout bounds the graceful drain of crawls and MCP sessions. A
	// non-positive value selects defaultShutdownTimeout.
	ShutdownTimeout time.Duration
}

type app struct {
	jobs    *jobManager
	runner  crawlRunner
	mcp     *mcpServer
	handler http.Handler

	// ctx is the parent of every launched crawl. cancel is called exactly once
	// by shutdown so in-flight runners observe the same interruption.
	ctx    context.Context
	cancel context.CancelFunc

	shutdownTimeout time.Duration

	// launchMu makes the shutting-down check and crawlWG.Add atomic, so
	// Shutdown can never race a new crawler into the wait group after it
	// starts draining.
	launchMu     sync.Mutex
	crawlWG      sync.WaitGroup
	shuttingDown atomic.Bool

	shutdownOnce sync.Once
	shutdownErr  error
}

// errShuttingDown is returned to a start request that arrives while the
// process is draining. The MCP adapter renders it as a retryable
// internal_error so clients back off instead of retrying the same request
// immediately.
var errShuttingDown = newRetryableToolError(codeInternalError,
	"server is shutting down; retry after it restarts",
	map[string]any{"reason": "shutting_down"})

// jobInterruptedMessage is the stable, client-visible explanation for a job
// stopped by shutdown.
const jobInterruptedMessage = "job interrupted by server shutdown"

// withArtifactBase derives the artifact origin from the incoming MCP request
// and stores it in the request context before the SDK handles the call. Tool
// handlers use it only when no configured base URL is set.
func withArtifactBase(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if base := requestArtifactBase(r); base != "" {
			r = r.WithContext(context.WithValue(r.Context(), artifactBaseContextKey{}, base))
		}
		next.ServeHTTP(w, r)
	})
}

// requestArtifactBase derives <scheme>://<host> from transport metadata only.
// It never reads the query or body, so callers cannot influence the origin.
func requestArtifactBase(r *http.Request) string {
	if r == nil {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := firstForwardedValue(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = strings.ToLower(proto)
	}
	host := firstForwardedValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return ""
	}
	return normalizeArtifactBase(scheme + "://" + host)
}

// firstForwardedValue returns the first value of a comma-separated forwarding
// header, matching the value the nearest proxy appended.
func firstForwardedValue(value string) string {
	if idx := strings.IndexByte(value, ','); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
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

	var store *jobStore
	if cfg.MCP.PersistJobs {
		var err error
		store, err = newJobStore(cfg.MCP.ResultsDir)
		if err != nil {
			return nil, fmt.Errorf("initialize job persistence: %w", err)
		}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = newStructuredLogger(os.Stderr, os.Getenv("GOFROG_LOG_FORMAT"))
	}

	jm := newJobManager(jobManagerConfig{
		Retention: retentionConfig{
			MCPTTL:             cfg.MCP.JobTTL,
			UITTL:              cfg.MCP.UITTL,
			ReapInterval:       cfg.MCP.ReapInterval,
			MaxRetainedMCPJobs: cfg.MCP.MaxRetainedJobs,
		},
		Store:  store,
		Logger: logger,
	})
	ctx, cancel := context.WithCancel(context.Background())
	timeout := cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	a := &app{
		jobs:            jm,
		runner:          runner,
		ctx:             ctx,
		cancel:          cancel,
		shutdownTimeout: timeout,
	}
	mux := http.NewServeMux()
	registerWebRoutes(mux, a)
	a.handler = mux

	if cfg.MCP.Enabled {
		ms, err := newMCPServer(jm, cfg.MCP, runner, cfg.SSRF)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("initialize MCP server: %w", err)
		}
		ms.app = a
		mux.Handle(cfg.MCP.Path, withArtifactBase(ms.Handler()))
		mux.Handle(artifactSubtreePattern(cfg.MCP.Path), ms.ArtifactHandler())
		a.mcp = ms
	}

	return a, nil
}

func (a *app) Handler() http.Handler {
	return a.handler
}

// launch starts one crawl in the background. It reports false when shutdown has
// already begun, in which case the caller must not start the work.
func (a *app) launch(run func(context.Context)) bool {
	a.launchMu.Lock()
	if a.shuttingDown.Load() {
		a.launchMu.Unlock()
		return false
	}
	a.crawlWG.Add(1)
	a.launchMu.Unlock()

	go func() {
		defer a.crawlWG.Done()
		run(a.ctx)
	}()
	return true
}

// Shutdown drains the application exactly once. It stops new starts, marks
// running and queued MCP jobs interrupted, cancels every runner context, lets
// in-flight work close its archive writers, then closes MCP sessions. Concurrent
// and repeated calls share the first result.
func (a *app) Shutdown(ctx context.Context) error {
	a.shutdownOnce.Do(func() {
		a.shutdownErr = a.shutdown(ctx)
	})
	return a.shutdownErr
}

func (a *app) shutdown(ctx context.Context) error {
	a.launchMu.Lock()
	a.shuttingDown.Store(true)
	a.launchMu.Unlock()

	if a.mcp != nil {
		a.mcp.beginShutdown()
	}

	// Mark the jobs before cancelling so a runner that wakes from cancellation
	// cannot replace the interrupted terminal state with a generic failure.
	for _, job := range a.jobs.list(jobFilter{Owner: ownerMCP}) {
		job.interrupt(jobInterruptedMessage)
	}

	if a.cancel != nil {
		a.cancel()
	}

	drained := make(chan struct{})
	go func() {
		a.crawlWG.Wait()
		close(drained)
	}()

	var err error
	select {
	case <-drained:
	case <-ctx.Done():
		err = ctx.Err()
	}

	if a.mcp != nil {
		if closeErr := a.mcp.Close(ctx); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

// Close releases application resources. It must be safe to call after the HTTP
// listener has stopped.
func (a *app) Close(ctx context.Context) error {
	if a.cancel != nil {
		a.cancel()
	}
	if a.mcp != nil {
		return a.mcp.Close(ctx)
	}
	return nil
}

// serve starts the shared application on the given address and drains it when
// ctx is canceled.
func serve(addr string, cfg MCPConfig) error {
	return serveContext(context.Background(), addr, cfg)
}

func serveContext(ctx context.Context, addr string, cfg MCPConfig) error {
	a, err := newApp(appConfig{MCP: cfg})
	if err != nil {
		return err
	}
	defer a.Close(context.Background())

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	if cfg.Enabled {
		limits := cfg.Limits.withDefaults()
		fmt.Printf("MCP limits: max_running_jobs=%d max_running_jobs_per_token=%d queue=%d rate_limit_per_minute=%d\n",
			limits.MaxConcurrentJobs,
			limits.MaxConcurrentJobsPerToken,
			limits.queueLimit(),
			limits.RateLimitPerMinute,
		)
	}
	fmt.Printf("go-frog web server listening on %s\n", addr)

	return serveHTTP(ctx, &http.Server{Handler: a.Handler()}, ln, a)
}

// serveHTTP serves ln until ctx is canceled, then drains the application and
// the HTTP server under the configured timeout. A nil return means the shutdown
// succeeded; a failed drain is returned so the process can exit non-zero.
func serveHTTP(ctx context.Context, srv *http.Server, ln net.Listener, a *app) error {
	served := make(chan error, 1)
	go func() {
		served <- srv.Serve(ln)
	}()

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	timeout := a.shutdownTimeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var wg sync.WaitGroup
	var appErr, httpErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		appErr = a.Shutdown(shutdownCtx)
	}()
	go func() {
		defer wg.Done()
		httpErr = srv.Shutdown(shutdownCtx)
	}()
	wg.Wait()

	if httpErr != nil && !errors.Is(httpErr, http.ErrServerClosed) {
		return httpErr
	}
	return appErr
}

// ── http server ──────────────────────────────────────────────────────────────

func registerWebRoutes(mux *http.ServeMux, a *app) {
	jm := a.jobs
	runner := a.runner

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
		if !a.launch(func(ctx context.Context) {
			runCrawlJobContext(ctx, job, settings, runner)
		}) {
			job.interrupt(jobInterruptedMessage)
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}

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

// runMCPCrawlJob is the MCP launch path. The quota reservation is acquired
// before the job is created; the deferred release covers done, error, and any
// future interrupted terminal state. Queued jobs wait for admission, then
// transition from queued to starting before the crawl begins.
func runMCPCrawlJob(ctx context.Context, job *crawlJob, settings WizardSettings, runner crawlRunner, reservation *quotaReservation) {
	defer reservation.Release()

	if reservation.queued {
		if !reservation.WaitContext(ctx) {
			// Shutdown marks the job interrupted before canceling, so this
			// failure is a no-op for an interrupted job and only reports a
			// genuinely abandoned queue entry.
			job.fail(fmt.Errorf("interrupted before start"))
			return
		}
		job.startFromQueue()
	}

	runCrawlJobContext(ctx, job, settings, runner)
}

// auditAnalyze is the audit pipeline entry point. It is a variable so tests
// can observe the strict/inclusive phase transitions without depending on a
// real archive. Production always uses auditpkg.Analyze.
var auditAnalyze = auditpkg.Analyze

// runCrawlJob is the single launch path shared by callers that have no
// cancelable context.
func runCrawlJob(job *crawlJob, settings WizardSettings, runner crawlRunner) {
	runCrawlJobContext(context.Background(), job, settings, runner)
}

// runCrawlJobContext is the single launch path shared by the web UI and the MCP
// tools. The context is threaded into the crawler so shutdown can stop new work
// and let in-flight requests finish.
func runCrawlJobContext(ctx context.Context, job *crawlJob, settings WizardSettings, runner crawlRunner) {
	job.setPhase(jobPhaseStarting)

	// Only MCP jobs carry resolved limits. UI and CLI runs keep their current
	// unbounded behavior.
	if job.owner == ownerMCP {
		limits := job.limits
		settings.limits = &limits
	}

	onPhase := func(phase string) { job.setPhase(phase) }

	var outcome crawlOutcome
	var err error
	switch settings.Mode {
	case 1:
		outcome, err = runner.Spider(ctx, settings, job.progress, onPhase)
	case 2:
		outcome, err = runner.List(ctx, settings.listURLs, settings, job.progress, onPhase)
	default:
		err = fmt.Errorf("unknown mode %d", settings.Mode)
	}
	if err != nil {
		job.setPartialResults(outcome.Pages)
		job.fail(err)
		return
	}
	job.setTruncation(outcome.Truncated, outcome.LimitReason)
	pages := outcome.Pages

	if settings.Audit {
		if outcome.Truncated {
			// A capped crawl leaves an incomplete archive. Never publish it as
			// a complete audit; keep the partial rows and explain the skip.
			job.finishWithAuditError(pages, fmt.Errorf("audit skipped: crawl truncated at %s", outcome.LimitReason))
			return
		}
		if err := ctx.Err(); err != nil {
			job.setPartialResults(pages)
			job.fail(err)
			return
		}

		// Mark the audit running before the phase write so the persisted
		// auditing_strict snapshot already reports audit.state=running.
		job.beginAudit()
		job.setPhase(jobPhaseAuditingStrict)
		result, auditErr := auditAnalyze(settings.AuditArchiveDir, auditpkg.Options{Filters: settings.AuditFilters})
		if auditErr != nil {
			job.finishWithAuditError(pages, fmt.Errorf("internal linking audit: %w", auditErr))
			return
		}
		if err := ctx.Err(); err != nil {
			job.setPartialResults(pages)
			job.fail(err)
			return
		}

		// Build the inclusive report from the same archive. It keeps the
		// strict run's navigation filters but counts every other
		// widget/listing link as content.
		job.setPhase(jobPhaseAuditingInclusive)
		inclusive, inclusiveErr := auditAnalyze(settings.AuditArchiveDir, auditpkg.Options{
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
