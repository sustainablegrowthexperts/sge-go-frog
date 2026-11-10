package main

import (
	"context"
	"sync"
)

// crawlOutcome is the result of one crawl run. Pages is always the partial
// result set; Truncated and LimitReason describe whether a cap stopped the
// crawl before it was exhausted.
type crawlOutcome struct {
	Pages       []Page
	Truncated   bool
	LimitReason string
}

// crawlRunner starts a crawl and returns its outcome. The interface exists so
// the MCP tools and the web UI share one pipeline while tests can substitute a
// deterministic fake. Context is threaded so a caller can bound the run.
type crawlRunner interface {
	Spider(ctx context.Context, settings WizardSettings, onProgress progressFunc) (crawlOutcome, error)
	List(ctx context.Context, urls []string, settings WizardSettings, onProgress progressFunc) (crawlOutcome, error)
}

// realCrawlRunner adapts the existing crawl implementations to crawlRunner.
type realCrawlRunner struct{}

func (realCrawlRunner) Spider(ctx context.Context, settings WizardSettings, onProgress progressFunc) (crawlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return crawlOutcome{}, err
	}
	outcome, err := runSpiderContext(ctx, settings, onProgress)
	if err != nil {
		return crawlOutcome{}, err
	}
	if err := ctx.Err(); err != nil && !outcome.Truncated {
		return crawlOutcome{}, err
	}
	return outcome, nil
}

func (realCrawlRunner) List(ctx context.Context, urls []string, settings WizardSettings, onProgress progressFunc) (crawlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return crawlOutcome{}, err
	}
	outcome, err := runListContext(ctx, urls, settings, onProgress)
	if err != nil {
		return crawlOutcome{}, err
	}
	if err := ctx.Err(); err != nil && !outcome.Truncated {
		return crawlOutcome{}, err
	}
	return outcome, nil
}

// fakeCrawlRunner is a controllable runner for tests. Spider and List report
// start on the started channel, emit configured progress, optionally block
// until Release is called, and then return the configured pages and error.
type fakeCrawlRunner struct {
	started chan struct{}
	release chan struct{}

	releaseOnce  sync.Once
	block        bool
	progressURLs []string
	pages        []Page
	truncated    bool
	limitReason  string
	err          error

	mu             sync.Mutex
	spiderCalls    int
	listCalls      int
	spiderSettings WizardSettings
	listSettings   WizardSettings
	listURLs       []string
}

func newFakeCrawlRunner() *fakeCrawlRunner {
	return &fakeCrawlRunner{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		block:   true,
	}
}

// newImmediateFakeRunner returns a runner that completes without waiting for
// Release, for tests that only care about validation or terminal states.
func newImmediateFakeRunner(pages []Page, err error) *fakeCrawlRunner {
	r := newFakeCrawlRunner()
	r.block = false
	r.pages = pages
	r.err = err
	return r
}

// Release unblocks a running fake. It is safe to call more than once.
func (r *fakeCrawlRunner) Release() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func (r *fakeCrawlRunner) signalStarted() {
	select {
	case r.started <- struct{}{}:
	default:
	}
}

func (r *fakeCrawlRunner) run(ctx context.Context, settings WizardSettings, urls []string, list bool, onProgress progressFunc) (crawlOutcome, error) {
	r.mu.Lock()
	if list {
		r.listCalls++
		r.listSettings = settings
		r.listURLs = append([]string(nil), urls...)
	} else {
		r.spiderCalls++
		r.spiderSettings = settings
	}
	block := r.block
	pages := r.pages
	truncated := r.truncated
	limitReason := r.limitReason
	err := r.err
	progressURLs := append([]string(nil), r.progressURLs...)
	r.mu.Unlock()

	for _, url := range progressURLs {
		if onProgress != nil {
			onProgress(url)
		}
	}
	r.signalStarted()

	if block {
		select {
		case <-r.release:
		case <-ctx.Done():
			return crawlOutcome{}, ctx.Err()
		}
	}
	return crawlOutcome{Pages: pages, Truncated: truncated, LimitReason: limitReason}, err
}

func (r *fakeCrawlRunner) Spider(ctx context.Context, settings WizardSettings, onProgress progressFunc) (crawlOutcome, error) {
	return r.run(ctx, settings, nil, false, onProgress)
}

func (r *fakeCrawlRunner) List(ctx context.Context, urls []string, settings WizardSettings, onProgress progressFunc) (crawlOutcome, error) {
	return r.run(ctx, settings, urls, true, onProgress)
}
