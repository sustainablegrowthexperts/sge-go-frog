package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpServerName    = "go-frog"
	mcpServerVersion = "0.1.0"
)

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
}

// newMCPServer builds the MCP server metadata, registers the tools, and wraps
// the Streamable HTTP handler. The handler is mounted on the existing
// application mux by newApp.
func newMCPServer(jm *jobManager, cfg MCPConfig, runner crawlRunner, policy *ssrfPolicy) (*mcpServer, error) {
	sdkServer := mcp.NewServer(&mcp.Implementation{
		Name:    mcpServerName,
		Version: mcpServerVersion,
	}, nil)
	sdkServer.AddReceivingMiddleware(normalizeSDKValidationErrors)

	if policy == nil {
		policy = newSSRFPolicy()
	}
	s := &mcpServer{
		server: sdkServer,
		jobs:   jm,
		config: cfg,
		runner: runner,
		quota:  newMCPQuota(cfg.Limits),
		ssrf:   policy,
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
	if idx := strings.Index(text, "unexpected additional properties ["); idx >= 0 {
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

// Handler returns the HTTP handler to mount at the configured MCP path.
func (s *mcpServer) Handler() http.Handler {
	return s.handler
}

// Close releases MCP sessions. The SDK manages per-request Streamable HTTP
// sessions, so there is no server-wide close in the pinned SDK version; this
// hook exists so startup/shutdown code has a single lifecycle entry point.
func (s *mcpServer) Close(_ context.Context) error {
	return nil
}
