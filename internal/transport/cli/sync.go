package cli

import (
	"github.com/spf13/cobra"

	"lore/internal/services"
)

func newSyncCommand(resolve Resolver, configPath *string) *cobra.Command {
	var reembed bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Run one sync round over every configured source",
		Long: "Streams each configured source's changes into the workspace index,\n" +
			"checkpointing per batch: an interrupted round resumes where it stopped.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				if err := rt.Sync.Sync(cmd.Context(), services.SyncOptions{Reembed: reembed}); err != nil {
					return err
				}
				printfln(cmd.OutOrStdout(), "sync complete — `lore status` for counts and cursor ages")
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&reembed, "reembed", false,
		"rebuild every chunk and vector against the configured embedder; needed after an embedder change")
	return cmd
}
