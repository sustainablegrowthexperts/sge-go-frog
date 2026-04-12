package main

import (
	"fmt"
	"os"
	"time"

	"github.com/schollz/progressbar/v3"
)

// newCrawlProgressBar builds a stderr progress bar. Pass listURLCount when list mode and count
// is known (>0); pass nil for spider mode (unknown total: spinner + page count).
//
// Spider mode (max=-1): progressbar sets ignoreLength and, by default, starts a background
// ticker while spinnerChangeInterval != 0. That ticker only stops when IsFinished() is true,
// but Exit() does not set finished — so the line keeps redrawing after the crawl. We set
// spinnerChangeInterval to 0 so that goroutine is never started; the bar still updates on each
// Add(). List mode uses a known max (ignoreLength false), so the library never starts that ticker.
func newCrawlProgressBar(listURLCount *int) *progressbar.ProgressBar {
	common := []progressbar.Option{
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetWidth(36),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
		progressbar.OptionSetItsString("pages"),
		progressbar.OptionShowTotalBytes(false),
		progressbar.OptionThrottle(80 * time.Millisecond),
		progressbar.OptionUseANSICodes(true),
		progressbar.OptionOnCompletion(func() {
			_, _ = fmt.Fprint(os.Stderr, "\n")
		}),
	}
	if listURLCount != nil && *listURLCount > 0 {
		return progressbar.NewOptions64(int64(*listURLCount),
			append([]progressbar.Option{
				progressbar.OptionSetDescription("Fetching URLs"),
				progressbar.OptionSetRenderBlankState(true),
			}, common...)...,
		)
	}
	return progressbar.NewOptions64(-1,
		append([]progressbar.Option{
			progressbar.OptionSetDescription("Crawling pages"),
			// Stops the indeterminate-mode background ticker (see package comment).
			progressbar.OptionSetSpinnerChangeInterval(0),
		}, common...)...,
	)
}

func finishCrawlProgressBar(bar *progressbar.ProgressBar, listURLCount *int) {
	if bar == nil {
		return
	}
	if listURLCount != nil && *listURLCount > 0 {
		_ = bar.Finish()
		return
	}
	_ = bar.Exit()
}
