package grpc

import (
	"context"
	"log/slog"
	"net"
	"time"

	grpclib "google.golang.org/grpc"

	lorev1 "lore/api/proto/lore/v1"
	"lore/internal/errors/internalerror"
	"lore/internal/transport"
)

const shutdownGrace = 5 * time.Second

// Blocks until ctx is done.
func Serve(ctx context.Context, listener net.Listener, svc transport.Services, log *slog.Logger) error {
	server := grpclib.NewServer()
	lorev1.RegisterQueryServiceServer(server, newQueryServer(svc, log))
	lorev1.RegisterSyncServiceServer(server, newSyncServer(svc, ctx.Done(), log))

	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()

	select {
	case err := <-stopped:
		return internalerror.NewInternalError("the gRPC server stopped serving", err)
	case <-ctx.Done():
		shutdown(server)

		return nil
	}
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
