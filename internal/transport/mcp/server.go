package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lore/internal/errors/internalerror"
	"lore/internal/transport"
)

const (
	serverName    = "lore"
	serverVersion = "v0.1.0"
)

// Blocks until ctx is cancelled or the client disconnects.
func Serve(ctx context.Context, svc transport.Services) error {
	return newServer(svc, diagnosticLogger()).Run(ctx, &sdk.StdioTransport{})
}

func newServer(svc transport.Services, log *slog.Logger) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: serverName, Version: serverVersion}, nil)
	registerFindDecision(server, svc.Query, log)
	registerWhy(server, svc.Why, log)
	registerTrace(server, svc.Trace, log)
	registerImpactOf(server, svc.Impact, log)
	registerHistoryOf(server, svc.History, log)
	registerSyncNow(server, svc.Sync, log)
	registerSyncStatus(server, svc.Status, log)

	return server
}

// Stdout carries the JSON-RPC stream, so anything printed there corrupts the session.
func diagnosticLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func toolError(log *slog.Logger, tool string, err error) error {
	kind, message := transport.Classify(err)

	switch kind {
	case internalerror.KindBadRequest:
		return fmt.Errorf("invalid argument: %s", message)
	case internalerror.KindNotFound:
		return fmt.Errorf("not found: %s", message)
	case internalerror.KindPrecondition:
		return errors.New(message)
	}

	log.Error(tool+" failed", "error", err)

	return errors.New(message)
}
