package mcp

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/secrets"
	"github.com/setthasit/Lore/internal/transport"
)

const EndpointPath = "/mcp"

const (
	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 5 * time.Second
)

// Blocks until ctx is done, answering over streamable HTTP the tool calls Serve
// answers over stdio.
func ServeHTTP(ctx context.Context, listener net.Listener, svc transport.Services, tlsConfig *tls.Config, sink *secrets.Sink, log *slog.Logger) error {
	tools := newServer(svc, log)

	mux := http.NewServeMux()
	// Stateless rejects every server-to-client request: no tool here makes one.
	mux.Handle(EndpointPath, sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return tools },
		&sdk.StreamableHTTPOptions{Stateless: true, Logger: log},
	))

	server := &http.Server{
		Handler:           scrubResponses(sink, mux),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	stopped := make(chan error, 1)
	go func() { stopped <- accept(server, listener, tlsConfig) }()

	select {
	case err := <-stopped:
		return internalerror.NewInternalError("the MCP HTTP server stopped serving", err)
	case <-ctx.Done():
		return shutdown(server)
	}
}

func accept(server *http.Server, listener net.Listener, tlsConfig *tls.Config) error {
	if tlsConfig != nil {
		return server.ServeTLS(listener, "", "")
	}
	return server.Serve(listener)
}

func scrubResponses(sink *secrets.Sink, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&scrubbingResponseWriter{ResponseWriter: w, body: sink.Writer(w)}, r)
	})
}

// Scrub sees one Write at a time; the SDK writes each JSON-RPC message and each SSE event whole.
type scrubbingResponseWriter struct {
	http.ResponseWriter
	body io.Writer
}

func (w *scrubbingResponseWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *scrubbingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// A tool call in flight keeps its connection for shutdownGrace so its answer still
// reaches the caller; past that it is dropped, and a stateless session loses nothing.
func shutdown(server *http.Server) error {
	grace, giveUp := context.WithTimeout(context.Background(), shutdownGrace)
	defer giveUp()

	if err := server.Shutdown(grace); err != nil {
		_ = server.Close()
		return internalerror.NewInternalError(
			"the MCP HTTP server dropped tool calls that were still running at shutdown", err)
	}
	return nil
}
