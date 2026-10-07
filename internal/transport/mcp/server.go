package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport"
)

const (
	serverName    = "lore"
	serverVersion = "v0.1.0"
)

// Blocks until ctx is cancelled or the client disconnects. Requests are read from os.Stdin.
func Serve(ctx context.Context, out io.Writer, svc transport.Services, log *slog.Logger) error {
	instructions := connectInstructions(ctx, svc.Status, log)
	return newServer(svc, log, instructions).Run(ctx, &sdk.IOTransport{Reader: os.Stdin, Writer: nopWriteCloser{out}})
}

func connectInstructions(ctx context.Context, status services.StatusService, log *slog.Logger) string {
	stats, err := status.Status(ctx)
	if err != nil {
		log.Error("reading index state for server instructions failed", "error", err)
		return serverRoutingInstructions + "\n" + unavailableIndexLine
	}
	return serverInstructions(stats, time.Now())
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func newServer(svc transport.Services, log *slog.Logger, instructions string) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: serverName, Version: serverVersion}, &sdk.ServerOptions{Instructions: instructions})
	registerFindDecision(server, svc.Query, log)
	registerWhy(server, svc.Why, log)
	registerTrace(server, svc.Trace, log)
	registerImpactOf(server, svc.Impact, log)
	registerHistoryOf(server, svc.History, log)
	registerSyncNow(server, svc.Sync, log)
	registerSyncStatus(server, svc.Status, log)

	return server
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
