package main

import (
	"context"
	"net/http"

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
}

// newMCPServer builds the MCP server metadata, registers the tools, and wraps
// the Streamable HTTP handler. The handler is mounted on the existing
// application mux by newApp.
func newMCPServer(jm *jobManager, cfg MCPConfig, runner crawlRunner) (*mcpServer, error) {
	sdkServer := mcp.NewServer(&mcp.Implementation{
		Name:    mcpServerName,
		Version: mcpServerVersion,
	}, nil)

	s := &mcpServer{
		server: sdkServer,
		jobs:   jm,
		config: cfg,
		runner: runner,
	}
	s.registerTools()

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return sdkServer
	}, &mcp.StreamableHTTPOptions{})

	s.handler = handler
	return s, nil
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
