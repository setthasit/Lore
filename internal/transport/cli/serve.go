package cli

import (
	"crypto/tls"
	"net"

	"github.com/spf13/cobra"

	"lore/internal/config"
	"lore/internal/di"
	"lore/internal/errors/internalerror"
	"lore/internal/transport/mcp"
)

func newServeCommand(resolve Resolver, configPath *string) *cobra.Command {
	httpAddr := new(string)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the lore daemon: serve MCP over HTTP and sync in the background",
		Long: "Serves the lore tool surface over MCP streamable HTTP at " + mcp.EndpointPath + " and runs\n" +
			"the sync scheduler alongside it, until interrupted. An address that is not\n" +
			"loopback is refused unless server.mtls.cert and server.mtls.key are set.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				return serveMCP(cmd, rt, *httpAddr)
			}, di.SchedulerModule)
		},
	}
	cmd.Flags().StringVar(httpAddr, "http", "", "address to serve MCP on, overriding server.http_addr")

	return cmd
}

func serveMCP(cmd *cobra.Command, rt *Runtime, override string) error {
	addr := override
	if addr == "" {
		addr = rt.Config.Server.HTTPAddr
	}
	if addr == "" {
		return internalerror.NewBadRequestError(
			"no address to serve on: set server.http_addr in lore.yaml or pass --http 127.0.0.1:8080", nil)
	}
	if err := rt.Config.ValidateListenAddr(addr); err != nil {
		return err
	}

	tlsConfig, err := serverTLS(rt.Config.Server.MTLS)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return internalerror.NewBadRequestError("could not listen on "+addr, err)
	}
	printfln(cmd.ErrOrStderr(), "lore: serving MCP on %s%s%s", scheme(tlsConfig), listener.Addr(), mcp.EndpointPath)

	return mcp.ServeHTTP(cmd.Context(), listener, rt.mcpServices(), tlsConfig)
}

func scheme(tlsConfig *tls.Config) string {
	if tlsConfig != nil {
		return "https://"
	}
	return "http://"
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
			"could not load the TLS certificate in server.mtls.cert and server.mtls.key", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}, nil
}
