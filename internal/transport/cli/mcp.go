package cli

import (
	"github.com/spf13/cobra"

	"lore/internal/transport/mcp"
)

func newMCPCommand(resolve Resolver, configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the lore tool surface over MCP stdio",
		Long: "Speaks MCP on stdin/stdout for a local agent harness, and returns when the\n" +
			"client disconnects or the process is interrupted. Nothing else may be printed\n" +
			"on stdout while it runs: the stream is the protocol.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				return mcp.Serve(cmd.Context(), rt.services())
			})
		},
	}
}
