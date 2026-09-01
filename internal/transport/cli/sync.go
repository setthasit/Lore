package cli

import (
	"time"

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
				res, err := rt.Sync.Sync(cmd.Context(), services.SyncOptions{Reembed: reembed})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if res.TookOverFrom != nil {
					printfln(out, "took over a dead sync lease from %s, last heartbeat %s",
						res.TookOverFrom.Holder, humanizeAge(time.Since(res.TookOverFrom.HeartbeatAt)))
				}
				printfln(out, "sync complete — `lore status` for counts and cursor ages")
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&reembed, "reembed", false,
		"rebuild every chunk and vector against the configured embedder; needed after an embedder change")
	return cmd
}
