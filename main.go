package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultConcurrency = 10

// WizardSettings holds answers from the interactive CLI (used by later phases).
type WizardSettings struct {
	Mode        int // 1 = Spider, 2 = List
	StartURL    string
	CSVPath     string
	KeywordsRaw string
	Concurrency int
}

func main() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println(styleTitle + "go-frog" + styleReset + " — site crawl & custom search")
	fmt.Println()

	mode := promptMode(reader)
	settings := WizardSettings{Mode: mode}

	switch mode {
	case 1:
		fmt.Println(styleNote + "The exact starting URL string matters: https:// and http:// are not the same," + styleReset)
		fmt.Println(styleNote + "and www.example.com is not the same host as example.com. Open the site in your" + styleReset)
		fmt.Println(styleNote + "browser, wait for the homepage to finish loading, then copy the URL from the address bar." + styleReset)
		fmt.Println()
		settings.StartURL = promptLine(reader, "Enter Starting URL (e.g., https://example.com):")
	case 2:
		fmt.Println(styleNote + "You can drag and drop the file from File Explorer or Finder into this terminal window." + styleReset)
		fmt.Println()
		settings.CSVPath = promptLine(reader, "Enter full path to your CSV file:")
	}

	settings.KeywordsRaw = promptOptionalLine(reader, "Enter Custom Search Keywords (separated by |) or leave blank:")
	settings.Concurrency = promptConcurrency(reader, "Maximum Concurrency (default 10):")

	printSummary(settings)

	var pages []Page
	var err error
	switch settings.Mode {
	case 1:
		pages, err = runSpider(settings)
	case 2:
		pages, err = runList(settings)
	default:
		err = fmt.Errorf("unknown mode %d", settings.Mode)
	}
	if err != nil {
		printErr("Crawl error: %v\n", err)
		os.Exit(1)
	}

	outFile := buildResultsFilename(settings, time.Now())
	if err := writeResultsCSV(outFile, pages, settings.KeywordsRaw); err != nil {
		printErr("Export error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%sCrawl complete!%s Results saved to %s%s%s\n", styleOK, styleReset, styleLabel, outFile, styleReset)
	fmt.Println("Press Enter to exit.")
	_, _ = reader.ReadString('\n')
}

func promptMode(reader *bufio.Reader) int {
	for {
		fmt.Println(styleLabel + "Choose Mode: (1) Spider Mode [Crawl a domain] or (2) List Mode [Process a CSV of URLs]" + styleReset)
		printPromptCaret()
		line := stripSurroundingQuotes(strings.TrimSpace(readLine(reader)))
		switch line {
		case "1":
			return 1
		case "2":
			return 2
		default:
			fmt.Println(styleWarn + "Please enter 1 or 2." + styleReset)
			fmt.Println()
		}
	}
}

func promptLine(reader *bufio.Reader, label string) string {
	for {
		fmt.Println(styleLabel + label + styleReset)
		printPromptCaret()
		s := stripSurroundingQuotes(strings.TrimSpace(readLine(reader)))
		if s != "" {
			return s
		}
		fmt.Println(styleWarn + "This field cannot be empty. Try again." + styleReset)
		fmt.Println()
	}
}

func promptOptionalLine(reader *bufio.Reader, label string) string {
	fmt.Println(styleLabel + label + styleReset)
	printPromptCaret()
	return stripSurroundingQuotes(strings.TrimSpace(readLine(reader)))
}

func promptConcurrency(reader *bufio.Reader, label string) int {
	for {
		fmt.Println(styleLabel + label + styleReset)
		printPromptCaret()
		line := stripSurroundingQuotes(strings.TrimSpace(readLine(reader)))
		if line == "" {
			return defaultConcurrency
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 {
			fmt.Println(styleWarn + "Enter a positive integer, or press Enter for the default." + styleReset)
			fmt.Println()
			continue
		}
		return n
	}
}

func readLine(reader *bufio.Reader) string {
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimSuffix(line, "\r\n")
}

// stripSurroundingQuotes removes one pair of matching ASCII quotes around the whole string.
// Windows terminals often paste drag-dropped paths as "C:\path\file name.csv", which would
// otherwise fail os.Open with the quotes included.
func stripSurroundingQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	switch {
	case s[0] == '"' && s[len(s)-1] == '"':
		return s[1 : len(s)-1]
	case s[0] == '\'' && s[len(s)-1] == '\'':
		return s[1 : len(s)-1]
	default:
		return s
	}
}

func printSummary(s WizardSettings) {
	fmt.Println()
	fmt.Println(styleHeading + "Configuration" + styleReset)
	dk := func(label string) string { return styleDim + label + styleReset }
	fmt.Printf("  %s          %d (%s)\n", dk("Mode:"), s.Mode, modeLabel(s.Mode))
	if s.Mode == 1 {
		fmt.Printf("  %s  %s\n", dk("Starting URL:"), s.StartURL)
	} else {
		fmt.Printf("  %s      %s\n", dk("CSV file:"), s.CSVPath)
	}
	if s.KeywordsRaw == "" {
		fmt.Printf("  %s      (none)\n", dk("Keywords:"))
	} else {
		fmt.Printf("  %s      %s\n", dk("Keywords:"), s.KeywordsRaw)
	}
	fmt.Printf("  %s   %d\n", dk("Concurrency:"), s.Concurrency)
	fmt.Println()
}

func printErr(format string, a ...interface{}) {
	if styleErr != "" {
		fmt.Fprintf(os.Stderr, styleErr+format+styleReset, a...)
		return
	}
	fmt.Fprintf(os.Stderr, format, a...)
}

func modeLabel(mode int) string {
	switch mode {
	case 1:
		return "Spider"
	case 2:
		return "List"
	default:
		return "unknown"
	}
}
