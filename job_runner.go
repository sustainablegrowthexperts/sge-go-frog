package main

import (
	"context"
	"sync"
)

// crawlRunner starts a crawl and returns its pages. The interface exists so the
// MCP tools and the web UI share one pipeline while tests can substitute a
// deterministic fake. Context is threaded now even though the crawlers only
// check it at their boundaries in this slice.
type crawlRunner interface {
	Spider(ctx context.Context, settings WizardSettings, onProgress progressFunc) ([]Page, error)
	List(ctx context.Context, urls []string, settings WizardSettings, onProgress progressFunc) ([]Page, error)
}

// realCrawlRunner adapts the existing crawl implementations to crawlRunner.
type realCrawlRunner struct{}

func (realCrawlRunner) Spider(ctx context.Context, settings WizardSettings, onProgress progressFunc) ([]Page, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pages, err := runSpider(settings, onProgress)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return pages, nil
}

func (realCrawlRunner) List(ctx context.Context, urls []string, settings WizardSettings, onProgress progressFunc) ([]Page, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pages, err := runList(urls, settings, onProgress)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return pages, nil
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

func (r *fakeCrawlRunner) run(ctx context.Context, settings WizardSettings, urls []string, list bool, onProgress progressFunc) ([]Page, error) {
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
			return nil, ctx.Err()
		}
	}
	return pages, err
}

func (r *fakeCrawlRunner) Spider(ctx context.Context, settings WizardSettings, onProgress progressFunc) ([]Page, error) {
	return r.run(ctx, settings, nil, false, onProgress)
}

func (r *fakeCrawlRunner) List(ctx context.Context, urls []string, settings WizardSettings, onProgress progressFunc) ([]Page, error) {
	return r.run(ctx, settings, urls, true, onProgress)
}
