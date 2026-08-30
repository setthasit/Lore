package cli

import (
	"github.com/spf13/cobra"

	"lore/internal/services"
)

type traceFlags struct {
	direction string
	raw       bool
}

func newTraceCommand(resolve Resolver, configPath *string) *cobra.Command {
	var flags traceFlags

	cmd := &cobra.Command{
		Use:   "trace <ref>",
		Short: "Print the provenance neighborhood of one document as a timeline",
		Long: "Resolves <ref> — a commit SHA, a PR or issue number, a ticket key, a document\n" +
			"URL or a document id — and prints everything linked to it in the order it\n" +
			"happened, each entry with its source URL. This is the depth-on-one-document\n" +
			"companion to `lore ask`, which searches the trail for breadth; --raw emits\n" +
			"the evidence bundle as JSON for scripting.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				bundle, err := rt.Trace.Trace(cmd.Context(), services.TraceRequest{
					Ref:       args[0],
					Direction: flags.direction,
				})
				if err != nil {
					return err
				}

				return emitBundle(cmd.OutOrStdout(), bundle, flags.raw, viewTimeline)
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&flags.direction, "direction", "",
		"which links to follow: out (the documents this one references), in (the documents that reference this one), both (default)")
	f.BoolVar(&flags.raw, "raw", false, "emit the evidence bundle as JSON instead of prose")
	return cmd
}
