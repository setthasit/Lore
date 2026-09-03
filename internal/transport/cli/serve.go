package cli

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"lore/internal/config"
	"lore/internal/di"
	"lore/internal/errors/internalerror"
	"lore/internal/transport/grpc"
	"lore/internal/transport/mcp"
)

const defaultGRPCAddr = "127.0.0.1:9090"

func newServeCommand(resolve Resolver, configPath *string) *cobra.Command {
	httpAddr, grpcAddr, mutual := new(string), new(string), new(bool)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the lore daemon: serve MCP over HTTP, the gRPC API, and sync in the background",
		Long: "Serves the lore tool surface over MCP streamable HTTP at " + mcp.EndpointPath + ", the lore.v1\n" +
			"gRPC API on " + defaultGRPCAddr + ", and runs the sync scheduler alongside them, until\n" +
			"interrupted. An address that is not loopback is refused unless server.mtls.cert\n" +
			"and server.mtls.key are set.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				return serve(cmd, rt, *httpAddr, *grpcAddr, *mutual)
			}, di.SchedulerModule)
		},
	}
	cmd.Flags().StringVar(httpAddr, "http", "", "address to serve MCP on, overriding server.http_addr")
	cmd.Flags().StringVar(grpcAddr, "grpc", "", "address to serve the gRPC API on, overriding server.grpc_addr")
	cmd.Flags().BoolVar(mutual, "mtls", false,
		"serve gRPC over TLS, requiring a client certificate signed by server.mtls.client_ca")

	return cmd
}

func serve(cmd *cobra.Command, rt *Runtime, httpOverride, grpcOverride string, mutual bool) error {
	httpAddr := cmp.Or(httpOverride, rt.Config.Server.HTTPAddr)
	if httpAddr == "" {
		return internalerror.NewBadRequestError(
			"no address to serve on: set server.http_addr in lore.yaml or pass --http 127.0.0.1:8080", nil)
	}
	grpcAddr := cmp.Or(grpcOverride, rt.Config.Server.GRPCAddr, defaultGRPCAddr)
	if err := rt.Config.ValidateListenAddr("server.http_addr", httpAddr); err != nil {
		return err
	}
	if err := rt.Config.ValidateListenAddr("server.grpc_addr", grpcAddr); err != nil {
		return err
	}

	httpTLS, err := serverTLS(rt.Config.Server.MTLS)
	if err != nil {
		return err
	}
	grpcTLS, err := grpcTransportTLS(rt.Config.Server.MTLS, httpTLS, mutual)
	if err != nil {
		return err
	}

	httpListener, err := listen(httpAddr)
	if err != nil {
		return err
	}
	defer func() { _ = httpListener.Close() }()

	grpcListener, err := listen(grpcAddr)
	if err != nil {
		return err
	}
	defer func() { _ = grpcListener.Close() }()

	printfln(cmd.ErrOrStderr(), "lore: serving MCP on %s%s%s", scheme(httpTLS), httpListener.Addr(), mcp.EndpointPath)
	printfln(cmd.ErrOrStderr(), "lore: serving gRPC on %s%s", grpcListener.Addr(), securityNote(grpcTLS))

	svc := rt.services()
	return serveBoth(cmd.Context(),
		func(ctx context.Context) error { return mcp.ServeHTTP(ctx, httpListener, svc, httpTLS) },
		func(ctx context.Context) error {
			return grpc.Serve(ctx, grpc.Config{
				Listener:  grpcListener,
				Services:  svc,
				Synthesis: rt.Synthesis,
				Log:       di.DiagnosticLogger(),
				TLS:       grpcTLS,
			})
		},
	)
}

func serveBoth(ctx context.Context, mcpServe, grpcServe func(context.Context) error) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	stopped := make(chan error, 2)
	go func() { stopped <- mcpServe(ctx) }()
	go func() { stopped <- grpcServe(ctx) }()

	err := <-stopped
	stop()
	if other := <-stopped; err == nil {
		err = other
	}
	return err
}

func listen(addr string) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, internalerror.NewBadRequestError("could not listen on "+addr, err)
	}
	return listener, nil
}

func scheme(tlsConfig *tls.Config) string {
	if tlsConfig != nil {
		return "https://"
	}
	return "http://"
}

func securityNote(tlsConfig *tls.Config) string {
	switch {
	case tlsConfig == nil:
		return ""
	case tlsConfig.ClientAuth == tls.RequireAndVerifyClientCert:
		return " over mTLS"
	default:
		return " over TLS"
	}
}

func serverTLS(mtls *config.MTLS) (*tls.Config, error) {
	if mtls == nil || (mtls.Cert == "" && mtls.Key == "") {
		return nil, nil
	}
	if mtls.Cert == "" || mtls.Key == "" {
		return nil, internalerror.NewBadRequestError(
			"server.mtls needs both cert and key to serve over TLS: set both,"+
				" or remove the block to serve in the clear", nil)
	}

	pair, err := tls.LoadX509KeyPair(mtls.Cert, mtls.Key)
	if err != nil {
		return nil, internalerror.NewBadRequestError(
			"could not load the certificate in server.mtls.cert ("+mtls.Cert+") with the key in"+
				" server.mtls.key ("+mtls.Key+"): "+err.Error(), err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}, nil
}

// A configured server.mtls.client_ca means verify clients, so it turns mutual TLS on
// with or without --mtls.
func grpcTransportTLS(mtls *config.MTLS, serverSide *tls.Config, asked bool) (*tls.Config, error) {
	requestedBy := ""
	switch {
	case asked:
		requestedBy = "--mtls"
	case mtls != nil && mtls.ClientCA != "":
		requestedBy = "server.mtls.client_ca"
	default:
		return serverSide.Clone(), nil
	}

	if missing := missingMTLSKeys(mtls); len(missing) > 0 {
		return nil, internalerror.NewPreconditionError(
			requestedBy+" needs "+strings.Join(missing, ", ")+" in lore.yaml: mutual TLS takes a server"+
				" certificate, its key, and the CA bundle that signs client certificates", nil)
	}

	clientCAs, err := clientCAPool(mtls.ClientCA)
	if err != nil {
		return nil, err
	}

	mutual := serverSide.Clone()
	mutual.ClientAuth = tls.RequireAndVerifyClientCert
	mutual.ClientCAs = clientCAs

	return mutual, nil
}

func missingMTLSKeys(mtls *config.MTLS) []string {
	if mtls == nil {
		mtls = new(config.MTLS)
	}

	var missing []string
	for _, setting := range []struct{ key, value string }{
		{"server.mtls.cert", mtls.Cert},
		{"server.mtls.key", mtls.Key},
		{"server.mtls.client_ca", mtls.ClientCA},
	} {
		if setting.value == "" {
			missing = append(missing, setting.key)
		}
	}
	return missing
}

func clientCAPool(path string) (*x509.CertPool, error) {
	bundle, err := os.ReadFile(path)
	if err != nil {
		return nil, internalerror.NewBadRequestError(
			"could not read the client CA bundle in server.mtls.client_ca ("+path+"): "+err.Error(), err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, internalerror.NewBadRequestError(
			"server.mtls.client_ca holds no PEM certificate, so no client could ever be trusted: "+path, nil)
	}
	return pool, nil
}
