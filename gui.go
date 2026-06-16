package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

func runGUI() {
	a := app.New()
	w := a.NewWindow("go-frog — Site Crawl & Custom Search")
	w.Resize(fyne.NewSize(660, 560))

	// ── Title ──────────────────────────────────────────────────
	titleLabel := widget.NewLabelWithStyle(
		"go-frog — site crawl & custom search",
		fyne.TextAlignCenter,
		fyne.TextStyle{Bold: true},
	)

	// ── Mode ───────────────────────────────────────────────────
	modeRadio := widget.NewRadioGroup([]string{
		"Spider — Crawl a domain",
		"List — Process a CSV of URLs",
	}, func(string) {})
	modeRadio.Horizontal = false
	modeRadio.SetSelected("Spider — Crawl a domain")

	// ── Spider URL ─────────────────────────────────────────────
	urlEntry := widget.NewEntry()
	urlEntry.SetPlaceHolder("https://example.com")

	// ── List CSV ───────────────────────────────────────────────
	csvEntry := widget.NewEntry()
	csvEntry.SetPlaceHolder("/path/to/urls.csv")
	csvBrowse := widget.NewButton("Browse\u2026", func() {
		dialog.NewFileOpen(func(r fyne.URIReadCloser, e error) {
			if e != nil || r == nil {
				return
			}
			csvEntry.SetText(r.URI().Path())
			_ = r.Close()
		}, w).Show()
	})
	csvRow := container.NewBorder(nil, nil, nil, csvBrowse, csvEntry)

	// ── Keywords ───────────────────────────────────────────────
	kwEntry := widget.NewEntry()
	kwEntry.SetPlaceHolder("keyword1|keyword2|keyword3  (optional, pipe-separated)")

	// ── Concurrency ────────────────────────────────────────────
	concEntry := widget.NewEntry()
	concEntry.SetText("10")

	// ── Output path ────────────────────────────────────────────
	outEntry := widget.NewEntry()
	outEntry.SetPlaceHolder("results/<auto-name>.csv  (leave blank for default)")
	outBrowse := widget.NewButton("Save As\u2026", func() {
		dialog.NewFileSave(func(wr fyne.URIWriteCloser, e error) {
			if e != nil || wr == nil {
				return
			}
			outEntry.SetText(wr.URI().Path())
			_ = wr.Close()
		}, w).Show()
	})
	outRow := container.NewBorder(nil, nil, nil, outBrowse, outEntry)

	// ── Mode toggle ────────────────────────────────────────────
	urlLabel := widget.NewLabel("Starting URL:")
	csvLabel := widget.NewLabel("CSV File:")
	kwLabel := widget.NewLabel("Keywords:")
	concLabel := widget.NewLabel("Concurrency:")
	outLabel := widget.NewLabel("Output CSV:")

	hideListFields := func(hide bool) {
		if hide {
			csvLabel.Hide()
			csvRow.Hide()
			urlLabel.Show()
			urlEntry.Show()
		} else {
			csvLabel.Show()
			csvRow.Show()
			urlLabel.Hide()
			urlEntry.Hide()
		}
	}
	modeRadio.OnChanged = func(s string) {
		hideListFields(strings.Contains(s, "Spider"))
	}
	hideListFields(true)

	// ── Progress area ──────────────────────────────────────────
	// Spider mode: indeterminate spinner + count + URL
	// List mode: determinate bar + count/total + URL
	progressSpinner := widget.NewProgressBarInfinite()
	progressSpinner.Hide()

	progressBar := widget.NewProgressBar()
	progressBar.Min = 0
	progressBar.Max = 1
	progressBar.Hide()

	progressCount := widget.NewLabel("")
	progressCount.Wrapping = fyne.TextTruncate

	progressURL := widget.NewLabel("")
	progressURL.Wrapping = fyne.TextTruncate
	progressURL.Hide()

	summaryLabel := widget.NewLabel("")
	summaryLabel.Wrapping = fyne.TextWrapWord
	summaryLabel.Hide()

	proxyEntry := widget.NewEntry()
	proxyEntry.SetPlaceHolder("http://user:pass@proxy.example.com:8080  (optional)")
	proxyRow := container.NewVBox(
		widget.NewLabel("Proxy URL:"),
		proxyEntry,
	)

	advanced := widget.NewAccordion(
		widget.NewAccordionItem("Advanced", proxyRow),
	)

	statusLabel := widget.NewLabel("")
	statusLabel.Wrapping = fyne.TextWrapWord

	// ── Run button ─────────────────────────────────────────────
	runBtn := widget.NewButton("Run Crawl", nil) // assigned below

	// ── Layout ─────────────────────────────────────────────────
	content := container.NewVBox(
		titleLabel,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Mode:", fyne.TextAlignLeading, fyne.TextStyle{Bold: false}),
		modeRadio,
		widget.NewSeparator(),
		urlLabel, urlEntry,
		csvLabel, csvRow,
		kwLabel, kwEntry,
		concLabel, concEntry,
		advanced,
		widget.NewSeparator(),
		outLabel, outRow,
		widget.NewSeparator(),
		runBtn,
		progressSpinner, progressBar, progressCount, progressURL,
		summaryLabel,
		statusLabel,
	)

	scroll := container.NewVScroll(content)
	w.SetContent(scroll)

	// ── Run callback ───────────────────────────────────────────
	runBtn.OnTapped = func() {
		isSpider := strings.Contains(modeRadio.Selected, "Spider")

		settings := WizardSettings{}
		if isSpider {
			settings.Mode = 1
			settings.StartURL = strings.TrimSpace(urlEntry.Text)
			if settings.StartURL == "" {
				statusLabel.SetText("Error: Starting URL is required.")
				return
			}
		} else {
			settings.Mode = 2
			settings.CSVPath = strings.TrimSpace(csvEntry.Text)
			if settings.CSVPath == "" {
				statusLabel.SetText("Error: CSV file path is required.")
				return
			}
			if _, err := os.Stat(settings.CSVPath); os.IsNotExist(err) {
				statusLabel.SetText("Error: file not found: " + settings.CSVPath)
				return
			}
		}

		settings.KeywordsRaw = strings.TrimSpace(kwEntry.Text)
		settings.ProxyURL = strings.TrimSpace(proxyEntry.Text)

		concText := strings.TrimSpace(concEntry.Text)
		if concText == "" {
			settings.Concurrency = defaultConcurrency
		} else {
			n, err := strconv.Atoi(concText)
			if err != nil || n < 1 {
				statusLabel.SetText("Error: Concurrency must be a positive integer.")
				return
			}
			settings.Concurrency = n
		}

		outPath := strings.TrimSpace(outEntry.Text)

		// ── Show configuration summary ─────────────────────────
		modeName := "Spider"
		target := settings.StartURL
		if !isSpider {
			modeName = "List"
			target = settings.CSVPath
		}
		kw := settings.KeywordsRaw
		if kw == "" {
			kw = "(none)"
		}
		summaryLabel.SetText(fmt.Sprintf(
			"Mode: %s\nTarget: %s\nKeywords: %s\nConcurrency: %d",
			modeName, target, kw, settings.Concurrency,
		))
		summaryLabel.Show()

		// ── Set up progress UI ─────────────────────────────────
		runBtn.Disable()
		statusLabel.SetText("")

		if isSpider {
			// Spider: indeterminate spinner + page count
			progressBar.Hide()
			progressSpinner.Show()
			progressURL.Show()
			progressCount.SetText("0 pages")
			progressURL.SetText("Starting crawl…")
		} else {
			// List: need to count URLs first
			urls, err := urlsFromCSV(settings.CSVPath)
			if err != nil {
				statusLabel.SetText("Error reading CSV: " + err.Error())
				runBtn.Enable()
				return
			}
			if len(urls) == 0 {
				statusLabel.SetText("Error: no URLs found in CSV.")
				runBtn.Enable()
				return
			}
			total := len(urls)
			progressSpinner.Hide()
			progressBar.Show()
			progressBar.Max = float64(total)
			progressBar.Min = 0
			progressBar.SetValue(0)
			progressURL.Show()
			progressCount.SetText(fmt.Sprintf("0 / %d", total))
			progressURL.SetText("Starting\u2026")

			// Run list crawl in background
			go func(urls []string) {
				var count int
				pages, err := runList(urls, settings, func(url string) {
					count++
					progressBar.SetValue(float64(count))
					progressCount.SetText(fmt.Sprintf("%d / %d", count, total))
					progressURL.SetText(url)
				})

				progressSpinner.Hide()
				progressBar.Hide()
				progressURL.Hide()
				progressCount.SetText("")

				if err != nil {
					runBtn.Enable()
					statusLabel.SetText(fmt.Sprintf("Crawl error: %v", err))
					return
				}

				if outPath == "" {
					outPath = buildResultsFilename(settings, time.Now())
				}
				if err := writeResultsCSV(outPath, pages, settings.KeywordsRaw); err != nil {
					runBtn.Enable()
					statusLabel.SetText(fmt.Sprintf("Export error: %v", err))
					return
				}

				runBtn.Enable()
				msg := fmt.Sprintf("✓ Complete! %d pages saved to:\n%s", len(pages), outPath)
				statusLabel.SetText(msg)
				a.SendNotification(fyne.NewNotification(
					"go-frog Complete",
					fmt.Sprintf("%d pages saved", len(pages)),
				))
			}(urls)
			return
		}

		// ── Run spider crawl in background ─────────────────────
		go func() {
			var count int
			pages, err := runSpider(settings, func(url string) {
				count++
				progressCount.SetText(fmt.Sprintf("%d pages", count))
				progressURL.SetText(url)
			})

			progressSpinner.Hide()
			progressURL.Hide()
			progressCount.SetText("")

			if err != nil {
				runBtn.Enable()
				statusLabel.SetText(fmt.Sprintf("Crawl error: %v", err))
				return
			}

			if outPath == "" {
				outPath = buildResultsFilename(settings, time.Now())
			}
			if err := writeResultsCSV(outPath, pages, settings.KeywordsRaw); err != nil {
				runBtn.Enable()
				statusLabel.SetText(fmt.Sprintf("Export error: %v", err))
				return
			}

			runBtn.Enable()
			msg := fmt.Sprintf("✓ Complete! %d pages saved to:\n%s", len(pages), outPath)
			statusLabel.SetText(msg)
			a.SendNotification(fyne.NewNotification(
				"go-frog Complete",
				fmt.Sprintf("%d pages saved", len(pages)),
			))
		}()
	}

	w.ShowAndRun()
}
