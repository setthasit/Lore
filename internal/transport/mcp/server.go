package mcp

import (
	"context"
	"log/slog"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lore/internal/services"
)

const (
	serverName    = "lore"
	serverVersion = "v0.1.0"
)

// Serve runs the Lore tool surface over stdio and blocks until ctx is
// cancelled or the client disconnects.
func Serve(ctx context.Context, query services.QueryService) error {
	return newServer(query, diagnosticLogger()).Run(ctx, &sdk.StdioTransport{})
}

func newServer(query services.QueryService, log *slog.Logger) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: serverName, Version: serverVersion}, nil)
	registerFindDecision(server, query, log)
	return server
}

// diagnosticLogger writes to stderr: stdout carries the JSON-RPC stream, and
// anything else printed there corrupts the session.
func diagnosticLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
