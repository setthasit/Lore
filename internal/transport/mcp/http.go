package mcp

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lore/internal/errors/internalerror"
)

const EndpointPath = "/mcp"

const (
	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 5 * time.Second
)

// Blocks until ctx is done, answering over streamable HTTP the tool calls Serve
// answers over stdio.
func ServeHTTP(ctx context.Context, listener net.Listener, svc Services, tlsConfig *tls.Config) error {
	log := diagnosticLogger()
	tools := newServer(svc, log)

	mux := http.NewServeMux()
	// Stateless rejects every server-to-client request: no tool here makes one.
	mux.Handle(EndpointPath, sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return tools },
		&sdk.StreamableHTTPOptions{Stateless: true, Logger: log},
	))

	server := &http.Server{Handler: mux, TLSConfig: tlsConfig, ReadHeaderTimeout: readHeaderTimeout}

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
