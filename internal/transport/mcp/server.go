package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lore/internal/errors/internalerror"
	"lore/internal/services"
)

const (
	serverName    = "lore"
	serverVersion = "v0.1.0"
)

// Causes name hosts, paths and queries, so they stay in the server log.
const internalErrorMessage = "internal error: see the lore server log for details"

type Services struct {
	Query  services.QueryService
	Trace  services.TraceService
	Impact services.ImpactService
}

// Blocks until ctx is cancelled or the client disconnects.
func Serve(ctx context.Context, svc Services) error {
	return newServer(svc, diagnosticLogger()).Run(ctx, &sdk.StdioTransport{})
}

func newServer(svc Services, log *slog.Logger) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: serverName, Version: serverVersion}, nil)
	registerFindDecision(server, svc.Query, log)
	registerTrace(server, svc.Trace, log)
	registerImpactOf(server, svc.Impact, log)

	return server
}

// Stdout carries the JSON-RPC stream, so anything printed there corrupts the session.
func diagnosticLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func toolError(log *slog.Logger, tool string, err error) error {
	var classified *internalerror.Error
	if errors.As(err, &classified) {
		switch classified.Kind {
		case internalerror.KindBadRequest:
			return fmt.Errorf("invalid argument: %s", classified.Message)
		case internalerror.KindNotFound:
			return fmt.Errorf("not found: %s", classified.Message)
		case internalerror.KindPrecondition:
			return errors.New(classified.Message)
		}
	}

	log.Error(tool+" failed", "error", err)

	return errors.New(internalErrorMessage)
}
