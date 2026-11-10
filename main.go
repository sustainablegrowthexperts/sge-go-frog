package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	auditpkg "go-frog/internal/audit"
)

const defaultConcurrency = 10

const (
	defaultMCPPath       = "/mcp"
	defaultMCPJobTTL     = 24 * time.Hour
	defaultMCPResultsDir = "results"
)

// MCPConfig is the configuration surface for the embedded MCP server. It is
// populated from defaults, environment variables, and command-line flags, in
// that order.
type MCPConfig struct {
	Enabled         bool
	Path            string
	JobTTL          time.Duration
	ResultsDir      string
	Limits          MCPLimits
	DefaultArchive  bool
	ArtifactBaseURL string
}

func defaultMCPConfig() MCPConfig {
	return MCPConfig{
		Enabled:         false,
		Path:            defaultMCPPath,
		JobTTL:          defaultMCPJobTTL,
		ResultsDir:      defaultMCPResultsDir,
		Limits:          defaultMCPLimits(),
		DefaultArchive:  false,
		ArtifactBaseURL: "",
	}
}

// mcpConfigFromEnv applies the GOFROG_MCP_* environment variables on top of
// the defaults. Flags are layered on afterwards by registerFlags.
func mcpConfigFromEnv(getenv func(string) string) (MCPConfig, error) {
	cfg := defaultMCPConfig()

	if v := strings.TrimSpace(getenv("GOFROG_MCP_ENABLED")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_ENABLED: %w", err)
		}
		cfg.Enabled = b
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_PATH")); v != "" {
		cfg.Path = v
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_JOB_TTL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_JOB_TTL: %w", err)
		}
		cfg.JobTTL = d
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_RESULTS_DIR")); v != "" {
		cfg.ResultsDir = v
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_PAGES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_PAGES: %w", err)
		}
		cfg.Limits.MaxPages = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_DEPTH")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_DEPTH: %w", err)
		}
		cfg.Limits.MaxDepth = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_LIST_URLS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_LIST_URLS: %w", err)
		}
		cfg.Limits.MaxListURLs = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_DURATION")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_DURATION: %w", err)
		}
		cfg.Limits.MaxDuration = d
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_JOBS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_JOBS: %w", err)
		}
		cfg.Limits.MaxConcurrentJobs = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_CONCURRENCY")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_CONCURRENCY: %w", err)
		}
		cfg.Limits.MaxConcurrency = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_KEYWORDS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_KEYWORDS: %w", err)
		}
		cfg.Limits.MaxKeywords = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_KEYWORD_LENGTH")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_KEYWORD_LENGTH: %w", err)
		}
		cfg.Limits.MaxKeywordLength = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_URL_LENGTH")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_URL_LENGTH: %w", err)
		}
		cfg.Limits.MaxURLLength = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_RESPONSE_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_RESPONSE_BYTES: %w", err)
		}
		cfg.Limits.MaxResponseBytes = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_REQUEST_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_REQUEST_BYTES: %w", err)
		}
		cfg.Limits.MaxRequestBytes = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_MAX_SEARCH_LIMIT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_MAX_SEARCH_LIMIT: %w", err)
		}
		cfg.Limits.MaxSearchLimit = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_RATE_LIMIT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_RATE_LIMIT: %w", err)
		}
		cfg.Limits.RateLimitPerMinute = n
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_DEFAULT_ARCHIVE")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("GOFROG_MCP_DEFAULT_ARCHIVE: %w", err)
		}
		cfg.DefaultArchive = b
	}
	if v := strings.TrimSpace(getenv("GOFROG_MCP_ARTIFACT_BASE_URL")); v != "" {
		cfg.ArtifactBaseURL = v
	}

	return cfg, nil
}

func (c *MCPConfig) registerFlags(fs *flag.FlagSet) {
	fs.BoolVar(&c.Enabled, "mcp", c.Enabled, "enable the MCP server")
	fs.StringVar(&c.Path, "mcp-path", c.Path, "HTTP path for the MCP endpoint")
	fs.DurationVar(&c.JobTTL, "mcp-job-ttl", c.JobTTL, "retention for MCP jobs")
	fs.StringVar(&c.ResultsDir, "mcp-results-dir", c.ResultsDir, "directory for MCP job artifacts")
	fs.IntVar(&c.Limits.MaxPages, "mcp-max-pages", c.Limits.MaxPages, "maximum pages per MCP crawl")
	fs.IntVar(&c.Limits.MaxDepth, "mcp-max-depth", c.Limits.MaxDepth, "maximum crawl depth for MCP jobs")
	fs.IntVar(&c.Limits.MaxListURLs, "mcp-max-list-urls", c.Limits.MaxListURLs, "maximum URLs in an MCP list job")
	fs.DurationVar(&c.Limits.MaxDuration, "mcp-max-duration", c.Limits.MaxDuration, "maximum duration for an MCP job")
	fs.IntVar(&c.Limits.MaxConcurrentJobs, "mcp-max-jobs", c.Limits.MaxConcurrentJobs, "maximum concurrent MCP jobs")
	fs.IntVar(&c.Limits.MaxConcurrency, "mcp-max-concurrency", c.Limits.MaxConcurrency, "maximum request concurrency for an MCP crawl")
	fs.IntVar(&c.Limits.MaxKeywords, "mcp-max-keywords", c.Limits.MaxKeywords, "maximum keywords per MCP crawl")
	fs.IntVar(&c.Limits.MaxKeywordLength, "mcp-max-keyword-length", c.Limits.MaxKeywordLength, "maximum characters per MCP keyword")
	fs.IntVar(&c.Limits.MaxURLLength, "mcp-max-url-length", c.Limits.MaxURLLength, "maximum URL length for MCP jobs")
	fs.Int64Var(&c.Limits.MaxResponseBytes, "mcp-max-response-bytes", c.Limits.MaxResponseBytes, "maximum MCP response size in bytes")
	fs.Int64Var(&c.Limits.MaxRequestBytes, "mcp-max-request-bytes", c.Limits.MaxRequestBytes, "maximum MCP request size in bytes")
	fs.IntVar(&c.Limits.MaxSearchLimit, "mcp-max-search-limit", c.Limits.MaxSearchLimit, "maximum search results per MCP request")
	fs.IntVar(&c.Limits.RateLimitPerMinute, "mcp-rate-limit", c.Limits.RateLimitPerMinute, "maximum MCP tool calls per minute")
	fs.BoolVar(&c.DefaultArchive, "mcp-default-archive", c.DefaultArchive, "archive HTML by default for MCP jobs")
	fs.StringVar(&c.ArtifactBaseURL, "mcp-artifact-base-url", c.ArtifactBaseURL, "public base URL for MCP artifacts")
}

func (c MCPConfig) validate() error {
	if c.Path == "" {
		return fmt.Errorf("MCP path must not be empty")
	}
	if !strings.HasPrefix(c.Path, "/") {
		return fmt.Errorf("MCP path %q must start with /", c.Path)
	}
	return nil
}

// WizardSettings holds all crawl configuration.
type WizardSettings struct {
	Mode        int // 1 = Spider, 2 = List
	StartURL    string
	CSVPath     string
	KeywordsRaw string
	Concurrency int
	ProxyURL    string // optional HTTP proxy (empty = env / none)

	// Audit is opt-in and spider-only. When true, the crawl also writes a
	// self-contained HTML archive to AuditArchiveDir for the internal-link
	// auditor. Existing CSV and page-row behavior is unchanged.
	Audit           bool
	AuditArchiveDir string
	AuditFilters    *auditpkg.FilterOptions

	listURLs []string
}

func main() {
	if err := runServer(os.Args[1:]); err != nil {
		printErr("go-frog: %v\n", err)
		os.Exit(1)
	}
}

// runServer parses the command line, applying environment-backed defaults, and
// starts either the CLI wizard or the HTTP server. Errors are returned so a
// failed listener or MCP mount is visible to the caller.
func runServer(args []string) error {
	// Preserve the existing --cli path: it must not depend on MCP
	// configuration being valid.
	for _, arg := range args {
		if arg == "--cli" {
			runCLI()
			return nil
		}
	}

	cfg, err := mcpConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("go-frog", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "HTTP listen address")
	cfg.registerFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	return serve(*addr, cfg)
}

func runCLI() {
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
		bar := newCrawlProgressBar(nil)
		defer finishCrawlProgressBar(bar, nil)
		pages, err = runSpider(settings, func(_ string) {
			_ = bar.Add(1)
		})
	case 2:
		urls, uErr := urlsFromCSV(settings.CSVPath)
		if uErr != nil {
			printErr("Reading CSV: %v\n", uErr)
			os.Exit(1)
		}
		if len(urls) == 0 {
			printErr("No URLs found in CSV\n")
			os.Exit(1)
		}
		n := len(urls)
		bar := newCrawlProgressBar(&n)
		defer finishCrawlProgressBar(bar, &n)
		pages, err = runList(urls, settings, func(_ string) {
			_ = bar.Add(1)
		})
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
