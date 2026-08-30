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

// Blocks until ctx is cancelled or the client disconnects.
func Serve(ctx context.Context, query services.QueryService) error {
	return newServer(query, diagnosticLogger()).Run(ctx, &sdk.StdioTransport{})
}

func newServer(query services.QueryService, log *slog.Logger) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: serverName, Version: serverVersion}, nil)
	registerFindDecision(server, query, log)
	return server
}

// Stdout carries the JSON-RPC stream, so anything printed there corrupts the session.
func diagnosticLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
