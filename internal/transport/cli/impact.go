package cli

import (
	"github.com/spf13/cobra"

	"lore/internal/services"
)

type impactFlags struct {
	question string
	raw      bool
}

func newImpactCommand(resolve Resolver, configPath *string) *cobra.Command {
	var flags impactFlags

	cmd := &cobra.Command{
		Use:   `impact <ref | "query">`,
		Short: "Print what followed a decision as a chronological timeline",
		Long: "Takes either a ref — a commit SHA, a PR or issue number, a ticket key, a\n" +
			"document URL or a document id — or free text naming the decision, and prints\n" +
			"the evidence that came after it in the order it happened, each entry with its\n" +
			"source URL; --raw emits the evidence bundle as JSON for scripting.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				bundle, err := rt.Impact.ImpactOf(cmd.Context(), services.ImpactRequest{
					Ref:      args[0],
					Question: flags.question,
				})
				if err != nil {
					return err
				}

				return emitBundle(cmd.OutOrStdout(), bundle, flags.raw, viewTimeline)
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&flags.question, "question", "", "narrow the timeline to the consequences this question asks about")
	f.BoolVar(&flags.raw, "raw", false, "emit the evidence bundle as JSON instead of prose")
	return cmd
}
