package server

import (
	"log/slog"
	"os"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/buffer"
	"github.com/mythingies/plugin-webex/internal/listener"
	"github.com/mythingies/plugin-webex/internal/router"
	"github.com/mythingies/plugin-webex/internal/tools"
	"github.com/mythingies/plugin-webex/internal/webex"
)

// Server wraps the MCP server and Webex client.
type Server struct {
	mcp      *mcpserver.MCPServer
	addr     string
	listener *listener.Listener
	buffer   *buffer.RingBuffer
	router   *router.Router
	client   *webex.Client
}

// New creates a new MCP server wired to Webex tools.
// configPath points to .webex-agents.yml; if empty or missing, defaults are used.
func New(token, addr, configPath string) (*Server, error) {
	client := webex.NewClient(token)

	// Load routing config (optional).
	var cfg *router.Config
	if configPath != "" {
		if _, err := os.Stat(configPath); err == nil {
			loaded, err := router.LoadConfig(configPath)
			if err != nil {
				slog.Warn("failed to load agent config, using defaults", "path", configPath, "error", err)
				cfg = router.DefaultConfig()
			} else {
				cfg = loaded
			}
		} else {
			cfg = router.DefaultConfig()
		}
	} else {
		cfg = router.DefaultConfig()
	}

	buf := buffer.New(cfg.Settings.BufferSize)
	rtr := router.NewRouter(cfg, configPath)
	lst := listener.New(token, client, buf, rtr)

	s := mcpserver.NewMCPServer(
		"webex",
		"1.0.0",
		mcpserver.WithToolCapabilities(true),
	)

	tools.Register(s, client, buf, rtr, lst)

	return &Server{
		mcp:      s,
		addr:     addr,
		listener: lst,
		buffer:   buf,
		router:   rtr,
		client:   client,
	}, nil
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
