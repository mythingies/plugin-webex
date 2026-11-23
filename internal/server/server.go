package server

import (
	"context"
	"errors"
	"io/fs"
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
// configPath points to .webex-agents.yml; see loadRoutingConfig.
func New(provider webex.TokenProvider, configPath string) (*Server, error) {
	client := webex.NewClient(provider)

	cfg := loadRoutingConfig(configPath)

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

// loadRoutingConfig loads the agent routing config. Only a file that does not
// exist (or an empty path) gets the built-in routes shipped with the plugin.
// A file that exists but can't be read or parsed gets defaults with no
// routes, so a broken user config is never silently replaced by the sample
// routes.
func loadRoutingConfig(configPath string) *router.Config {
	if configPath == "" {
		return router.BuiltinConfig()
	}
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		slog.Info("no agent config found, using built-in routes", "path", configPath)
		return router.BuiltinConfig()
	}
	cfg, err := router.LoadConfig(configPath)
	if err != nil {
		slog.Warn("failed to load agent config, routing disabled", "path", configPath, "error", err)
		return router.DefaultConfig()
	}
	return cfg
}
