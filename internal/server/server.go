package server

import (
	"github.com/ecopelan/plugin-webex/internal/tools"
	"github.com/ecopelan/plugin-webex/internal/webex"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Server wraps the MCP server and Webex client.
type Server struct {
	mcp  *mcpserver.MCPServer
	addr string
}

// New creates a new MCP server wired to Webex tools.
func New(token, addr string) (*Server, error) {
	client := webex.NewClient(token)

	s := mcpserver.NewMCPServer(
		"webex",
		"0.1.0",
		mcpserver.WithToolCapabilities(true),
	)

	tools.Register(s, client)

	return &Server{mcp: s, addr: addr}, nil
}

// Start begins serving MCP over HTTP.
func (s *Server) Start() error {
	httpServer := mcpserver.NewStreamableHTTPServer(s.mcp)
	return httpServer.Start(s.addr)
}

// MCPServer returns the underlying MCP server for testing.
func (s *Server) MCPServer() *mcpserver.MCPServer {
	return s.mcp
}
