package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tested MCP protocol revision: 2025-06-18, negotiated by the pinned
// modelcontextprotocol/go-sdk v1.8.0. Server metadata exposes the go-frog build
// version plus the independent MCP tool-schema version; archive and audit
// artifact versions intentionally do not share this value.
const (
	mcpServerName        = "go-frog"
	mcpBuildVersion      = "0.1.0"
	mcpToolSchemaVersion = "1.0.0"
	mcpServerVersion     = mcpBuildVersion + "+mcp." + mcpToolSchemaVersion
)

// mcpInstructions is the concise server-level guidance returned by initialize.
// It describes the start -> poll -> read -> download workflow and the artifact
// surface without duplicating each tool's description.
const mcpInstructions = `go-frog crawls public websites in spider or list mode and can audit internal linking.

Workflow: call start_crawl, then poll get_job until the job reaches a terminal status (done, error, or cancelled). Do not assume results are immediately available; use poll_after_ms from start_crawl or get_job between polls.

After completion:
- get_results returns filtered page rows and keyword counts.
- get_audit returns strict or inclusive audit sections.
- search_pages searches page metadata and, when archived, raw HTML or visible text.
- get_artifact returns a download URL for a generated file; you may also GET that absolute URL directly.

Prerequisites:
- HTML and text search requires archive_html=true or audit=true.
- Audit retrieval requires audit=true on a spider job.
- Artifacts are available only when the job status is done.

Artifacts: csv, audit_strict_zip, audit_inclusive_zip, audit_json_strict, audit_json_inclusive, audit_xlsx_strict, audit_xlsx_inclusive. URLs returned by start_crawl and get_artifact are absolute, job-scoped, and under the configured MCP endpoint. Downloads support GET and HEAD; URLs expire with the job retention window. Artifact downloads return 404 for unknown or expired URLs, 409 for unfinished jobs, and 405 for unsupported methods. Page, duration, size, and retention limits are server-enforced.`

// mcpServer is the thin adapter around the official MCP Go SDK. SDK types and
// transport details stay in this file so the crawl/job code does not depend on
// the protocol library.
type mcpServer struct {
	server  *mcp.Server
	handler http.Handler

	jobs   *jobManager
	config MCPConfig
	runner crawlRunner
	quota  *mcpQuota
	ssrf   *ssrfPolicy
	logger *slog.Logger

	// app is set when the adapter is mounted on a shared application; it owns
	// the shutdown context injected into launched crawls.
	app *app

	shuttingDown atomic.Bool
	closeOnce    sync.Once
}

// newStructuredLogger builds the process logger. Text is the default format;
// GOFROG_LOG_FORMAT=json selects the JSON handler. The service/version fields
// are attached once so every record is attributable.
func newStructuredLogger(out io.Writer, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var handler slog.Handler
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		handler = slog.NewJSONHandler(out, opts)
	} else {
		handler = slog.NewTextHandler(out, opts)
	}
	return slog.New(handler).With(
		"service", mcpServerName,
		"version", mcpServerVersion,
		"mcp_schema_version", mcpToolSchemaVersion,
	)
}

// loggerOr returns a usable logger for zero-value or test-constructed servers.
func loggerOr(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.Default()
}

// toolCallLog is the stable field set for one MCP tool invocation. Optional
// fields are added only when they are known, so operators can query every
// record without seeing empty values.
type toolCallLog struct {
	Tool       string
	JobID      string
	TargetHost string
	Outcome    string
	ErrorCode  string
	ErrorCause string
	Duration   time.Duration
	Pages      int
	HasPages   bool
	Bytes      int64
	HasBytes   bool
}

// logToolCall writes one structured tool-call record.
func logToolCall(logger *slog.Logger, rec toolCallLog) {
	attrs := []any{
		"tool", rec.Tool,
		"outcome", rec.Outcome,
		"duration_ms", rec.Duration.Milliseconds(),
	}
	if rec.JobID != "" {
		attrs = append(attrs, "job_id", rec.JobID)
	}
	if rec.TargetHost != "" {
		attrs = append(attrs, "target_host", rec.TargetHost)
	}
	if rec.ErrorCode != "" {
		attrs = append(attrs, "error_code", rec.ErrorCode)
	}
	if rec.ErrorCause != "" {
		attrs = append(attrs, "error", rec.ErrorCause)
	}
	if rec.HasPages {
		attrs = append(attrs, "pages", rec.Pages)
	}
	if rec.HasBytes {
		attrs = append(attrs, "bytes", rec.Bytes)
	}
	if rec.Outcome == "error" {
		loggerOr(logger).Warn("mcp tool call", attrs...)
		return
	}
	loggerOr(logger).Info("mcp tool call", attrs...)
}

// hostFromURL returns only the host name, never the path, query, or userinfo.
func hostFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// newMCPServer builds the MCP server metadata, registers the tools, and wraps
// the Streamable HTTP handler. The handler is mounted on the existing
// application mux by newApp.
func newMCPServer(jm *jobManager, cfg MCPConfig, runner crawlRunner, policy *ssrfPolicy) (*mcpServer, error) {
	sdkServer := mcp.NewServer(&mcp.Implementation{
		Name:    mcpServerName,
		Version: mcpServerVersion,
	}, &mcp.ServerOptions{Instructions: mcpInstructions})
	sdkServer.AddReceivingMiddleware(normalizeSDKValidationErrors)

	if policy == nil {
		policy = newSSRFPolicy()
	}
	logger := slog.Default()
	if jm != nil {
		logger = loggerOr(jm.logger)
	}
	s := &mcpServer{
		server: sdkServer,
		jobs:   jm,
		config: cfg,
		runner: runner,
		quota:  newMCPQuota(cfg.Limits, logger),
		ssrf:   policy,
		logger: logger,
	}
	s.registerTools()

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return sdkServer
	}, &mcp.StreamableHTTPOptions{})

	// Reject oversized bodies before the SDK decodes them or any tool can run.
	s.handler = limitMCPRequestBody(handler, cfg.Limits.withDefaults().MaxRequestBytes)
	return s, nil
}

// normalizeSDKValidationErrors converts the SDK's schema-validation result
// into the same stable envelope every tool handler uses. The SDK rejects
// unknown fields and malformed types before the handler runs, so this
// middleware is the only place those failures can be normalized.
func normalizeSDKValidationErrors(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		// A client may send "arguments": null for a tool whose fields are all
		// optional. The SDK then applies schema defaults to a nil map and panics
		// while writing the first default. Treat it as an empty object, which is
		// the same meaning the handler gives omitted arguments.
		if method == "tools/call" {
			if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil {
				args := bytes.TrimSpace(call.Params.Arguments)
				if len(args) == 0 || bytes.Equal(args, []byte("null")) {
					call.Params.Arguments = json.RawMessage(`{}`)
				}
			}
		}

		result, err := next(ctx, method, req)
		if err != nil || method != "tools/call" {
			return result, err
		}
		callResult, ok := result.(*mcp.CallToolResult)
		if !ok || !callResult.IsError {
			return result, err
		}
		text := firstTextContent(callResult)
		if isStableToolErrorText(text) {
			return result, err
		}
		normalized := normalizeToolValidationError(text)
		callResult.Content = []mcp.Content{&mcp.TextContent{Text: normalized.Error()}}
		return callResult, nil
	}
}

// firstTextContent returns the first text block from a tool result.
func firstTextContent(result *mcp.CallToolResult) string {
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

// isStableToolErrorText reports whether text already has the shared envelope.
func isStableToolErrorText(text string) bool {
	return strings.Contains(text, "; retryable=") && strings.Contains(text, "; details=")
}

// normalizeToolValidationError extracts a field name from the SDK validation
// message without echoing the caller's values. Unknown fields and type errors
// both become a field-named invalid_argument.
func normalizeToolValidationError(text string) *toolError {
	field := "arguments"
	if idx := strings.Index(text, "required: missing properties: ["); idx >= 0 {
		rest := strings.TrimLeft(text[idx+len("required: missing properties: ["):], `"`)
		if end := strings.IndexAny(rest, `",]`); end > 0 {
			field = rest[:end]
		}
	} else if idx := strings.Index(text, "unexpected additional properties ["); idx >= 0 {
		rest := strings.TrimLeft(text[idx+len("unexpected additional properties ["):], `"`)
		if end := strings.IndexAny(rest, `",]`); end > 0 {
			field = rest[:end]
		}
	} else if idx := strings.Index(text, "/properties/"); idx >= 0 {
		rest := text[idx+len("/properties/"):]
		if end := strings.IndexAny(rest, ":/ "); end > 0 {
			field = rest[:end]
		}
	}
	return newToolError(codeInvalidArgument, fmt.Sprintf("invalid or unknown argument %q", field), map[string]any{"field": field})
}

// limitMCPRequestBody reads the request body under a hard byte cap and gives
// the SDK a replayable body on success. Oversized requests get a protocol-level
// rejection, so a malformed or hostile payload never reaches tool execution.
func limitMCPRequestBody(next http.Handler, maxBytes int64) http.Handler {
	if maxBytes <= 0 {
		maxBytes = defaultMCPMaxRequestBytes
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBytes {
			writeMCPRequestTooLarge(w, maxBytes)
			return
		}
		if r.Body != nil {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
			if err != nil {
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					writeMCPRequestTooLarge(w, maxBytes)
					return
				}
				writeMCPProtocolError(w, http.StatusBadRequest, "could not read the request body")
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		next.ServeHTTP(w, r)
	})
}

// writeMCPRequestTooLarge emits a stable JSON-RPC error without echoing the
// body or any request headers.
func writeMCPRequestTooLarge(w http.ResponseWriter, maxBytes int64) {
	writeMCPProtocolError(w, http.StatusRequestEntityTooLarge,
		fmt.Sprintf("request body exceeds the maximum of %d bytes", maxBytes))
}

func writeMCPProtocolError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error": map[string]any{
			"code":    -32600,
			"message": message,
		},
	})
}

// artifactBaseContextKey carries the request-derived artifact origin from the
// transport middleware into every tool handler. Configuration still wins when
// a base URL is set.
type artifactBaseContextKey struct{}

// artifactBaseFromContext returns the request-derived origin, or "" when the
// request did not pass through withArtifactBase.
func artifactBaseFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	base, _ := ctx.Value(artifactBaseContextKey{}).(string)
	return normalizeArtifactBase(base)
}

// artifactBase resolves the origin for this tool call. The configured base URL
// wins; otherwise the incoming MCP request supplies scheme and host. When
// neither is available the result is empty rather than a relative path.
func (s *mcpServer) artifactBase(ctx context.Context) string {
	if base := normalizeArtifactBase(s.config.ArtifactBaseURL); base != "" {
		return base
	}
	return artifactBaseFromContext(ctx)
}

// mcpPath returns the normalized MCP mount prefix used in artifact URLs.
func (s *mcpServer) mcpPath() string {
	return normalizeMCPPath(s.config.Path)
}

// Handler returns the HTTP handler to mount at the configured MCP path.
func (s *mcpServer) Handler() http.Handler {
	return s.handler
}

// ArtifactHandler returns the HTTP handler for the job-scoped artifact
// subtree. It is separate from the Streamable HTTP handler.
func (s *mcpServer) ArtifactHandler() http.Handler {
	return http.HandlerFunc(s.serveArtifactDownload)
}

// beginShutdown makes every new start_crawl request fail with the stable
// retryable shutdown error. It is safe to call more than once.
func (s *mcpServer) beginShutdown() {
	if s == nil {
		return
	}
	s.shuttingDown.Store(true)
}

// Close releases MCP sessions. Closing is idempotent so the shutdown path and
// the deferred cleanup can both call it.
func (s *mcpServer) Close(_ context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		for session := range s.server.Sessions() {
			_ = session.Close()
		}
	})
	return nil
}
