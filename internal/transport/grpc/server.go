package grpc

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"time"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	lorev1 "github.com/setthasit/Lore/api/proto/lore/v1"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport"
)

const shutdownGrace = 5 * time.Second

type Config struct {
	Listener  net.Listener
	Services  transport.Services
	Synthesis services.SynthesisService
	Log       *slog.Logger
	TLS       *tls.Config
}

// Blocks until ctx is done. A nil Config.TLS serves in the clear.
func Serve(ctx context.Context, cfg Config) error {
	server := grpclib.NewServer(transportCredentials(cfg.TLS)...)
	lorev1.RegisterQueryServiceServer(server, newQueryServer(cfg.Services, cfg.Synthesis, cfg.Log))
	lorev1.RegisterSyncServiceServer(server, newSyncServer(cfg.Services, ctx.Done(), cfg.Log))

	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(cfg.Listener) }()

	select {
	case err := <-stopped:
		return internalerror.NewInternalError("the gRPC server stopped serving", err)
	case <-ctx.Done():
		shutdown(server)

		return nil
	}
}

func transportCredentials(tlsConfig *tls.Config) []grpclib.ServerOption {
	if tlsConfig == nil {
		return nil
	}
	return []grpclib.ServerOption{grpclib.Creds(credentials.NewTLS(tlsConfig))}
}

// A stream wedged in Send never returns, so GracefulStop needs a deadline behind it.
func shutdown(server *grpclib.Server) {
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		server.GracefulStop()
	}()

	select {
	case <-drained:
	case <-time.After(shutdownGrace):
		server.Stop()
		<-drained
	}
}
