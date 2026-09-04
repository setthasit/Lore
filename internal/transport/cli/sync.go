package cli

import (
	"errors"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/services"
)

func newSyncCommand(resolve Resolver, configPath *string) *cobra.Command {
	var (
		reembed bool
		source  string
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Run one sync round over every configured source",
		Long: "Streams each configured source's changes into the workspace index,\n" +
			"checkpointing per batch: an interrupted round resumes where it stopped.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				res, err := rt.Sync.Sync(cmd.Context(), services.SyncOptions{Source: source, Reembed: reembed})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if res.TookOverFrom != nil {
					printfln(out, "took over a dead sync lease from %s, last heartbeat %s",
						res.TookOverFrom.Holder, humanizeAge(time.Since(res.TookOverFrom.HeartbeatAt)))
				}
				if len(res.Failures) > 0 {
					return partialSync(out, res.Failures)
				}
				printfln(out, "sync complete — `lore status` for counts and cursor ages")
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&reembed, "reembed", false,
		"rebuild every chunk and vector against the configured embedder; needed after an embedder change")
	cmd.Flags().StringVar(&source, "source", "",
		"sync only this source instance, by the id it has in lore.yaml; omit it to sync every configured source")
	return cmd
}

// Instances fail independently, so a round with a failed instance still
// committed everything the others produced. The exit status is non-zero anyway:
// a workspace that could not read a source is not up to date, and a script has
// to be able to tell.
func partialSync(out io.Writer, failures []services.InstanceFailure) error {
	for _, failure := range failures {
		printfln(out, "%s failed at its last checkpoint — %s", failure.Instance, actionableMessage(failure.Err))
	}
	printfln(out, "the remaining sources are committed; `lore status` for counts and cursor ages")

	return internalerror.NewInternalError(pluralize(len(failures), "source", "sources")+
		" did not finish this round", errors.Join(errs(failures)...))
}

func errs(failures []services.InstanceFailure) []error {
	out := make([]error, len(failures))
	for i, failure := range failures {
		out[i] = failure.Err
	}
	return out
}
