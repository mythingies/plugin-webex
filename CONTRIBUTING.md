# Contributing to plugin-webex

## Setup

1. Install Go 1.22+
2. Install golangci-lint: `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest`
3. Clone the repo and run `make build`

## Workflow

1. Create a feature branch from `main`
2. Make changes
3. Run `make lint` and `make test` — both must pass
4. Commit with a descriptive message
5. Open a PR — GitHub Actions will verify lint + build

## Code Style

- Follow standard Go conventions (`gofmt`, `goimports`)
- Use `golangci-lint` for static analysis
- Keep packages focused: one responsibility per package
- Internal packages go in `internal/` — not importable by external consumers

## Testing

```bash
make test              # All tests
make test T=TestName   # Single test
```

## Project Layout

- `cmd/webex-mcp/` — Binary entry point
- `internal/server/` — MCP server wiring
- `internal/webex/` — Webex REST API client
- `internal/tools/` — MCP tool implementations
- `internal/listener/` — WebSocket listener (v0.2)
- `internal/buffer/` — Notification ring buffer (v0.2)
- `internal/router/` — Agent routing engine (v0.2)
