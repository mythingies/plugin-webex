package server

import (
	"context"
	"log/slog"
	"os"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/buffer"
	"github.com/mythingies/plugin-webex/internal/listener"
	"github.com/mythingies/plugin-webex/internal/router"
	"github.com/mythingies/plugin-webex/internal/tools"
	"github.com/mythingies/plugin-webex/internal/triage"
	"github.com/mythingies/plugin-webex/internal/webex"
)

// Server wraps the MCP server and Webex client.
type Server struct {
	mcp      *mcpserver.MCPServer
	listener *listener.Listener
	buffer   *buffer.RingBuffer
	router   *router.Router
	client   *webex.Client
	triage   *triage.Store
}

// New creates a new MCP server wired to Webex tools.
// configPath points to .webex-agents.yml; if empty or missing, defaults are used.
func New(provider webex.TokenProvider, configPath string) (*Server, error) {
	client := webex.NewClient(provider)

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
	lst := listener.New(provider, client, buf, rtr)

	// Durable "still to process" reminder list. Degrades gracefully: if it
	// can't be created we log and continue without it rather than fail to
	// serve.
	tri, err := triage.New()
	if err != nil {
		slog.Warn("triage store unavailable, continuing without persistent reminders", "error", err)
	} else {
		lst.SetTriageStore(tri)
	}

	s := mcpserver.NewMCPServer(
		"webex",
		"1.0.0",
		mcpserver.WithToolCapabilities(true),
	)

	tools.Register(s, client, buf, rtr, lst, tri)

	return &Server{
		mcp:      s,
		listener: lst,
		buffer:   buf,
		router:   rtr,
		client:   client,
		triage:   tri,
	}, nil
}

// Start serves MCP over stdin/stdout.
func (s *Server) Start(ctx context.Context) error {
	stdio := mcpserver.NewStdioServer(s.mcp)
	return stdio.Listen(ctx, os.Stdin, os.Stdout)
}

// MCPServer returns the underlying MCP server for testing.
func (s *Server) MCPServer() *mcpserver.MCPServer {
	return s.mcp
}
