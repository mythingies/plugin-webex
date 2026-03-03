package main

import (
	"fmt"
	"log"
	"os"

	"github.com/mythingies/plugin-webex/internal/server"
)

func main() {
	token := os.Getenv("WEBEX_TOKEN")
	if token == "" {
		log.Fatal("WEBEX_TOKEN environment variable is required. Generate one at https://developer.webex.com/docs/getting-your-personal-access-token")
	}

	addr := os.Getenv("WEBEX_MCP_ADDR")
	if addr == "" {
		addr = ":3119"
	}

	configPath := os.Getenv("WEBEX_AGENTS_CONFIG")
	if configPath == "" {
		configPath = ".webex-agents.yml"
	}

	srv, err := server.New(token, addr, configPath)
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	fmt.Fprintf(os.Stderr, "webex-mcp server listening on %s\n", addr)
	if err := srv.Start(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
