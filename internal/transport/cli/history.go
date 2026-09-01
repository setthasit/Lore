package cli

import (
	"github.com/spf13/cobra"

	"lore/internal/services"
)

type historyFlags struct {
	repo   string
	limit  int
	before string
	raw    bool
}

func newHistoryCommand(resolve Resolver, configPath *string) *cobra.Command {
	var flags historyFlags

	cmd := &cobra.Command{
		Use:   "history <path>",
		Short: "Print how a file evolved, as a timeline",
		Long: "Walks the commits that touched <path> in a registered local clone and prints\n" +
			"the trail behind them in the order it happened, each entry with its source\n" +
			"URL. Name the clone with --repo when the workspace registers more than one.\n" +
			"\n" +
			"One page at a time, newest commits first: --limit sizes the page and the\n" +
			"server caps it. To walk further back, pass the last commit of the `blamed`\n" +
			"line to --before; when a page blames nothing, the history is exhausted.\n" +
			"\n" +
			"A workspace that registers no clone cannot anchor on code at all: ask\n" +
			"`lore ask` there instead; --raw emits the evidence bundle as JSON for\n" +
			"scripting.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				bundle, err := rt.History.HistoryOf(cmd.Context(), services.HistoryRequest{
					Repo:   flags.repo,
					File:   args[0],
					Limit:  flags.limit,
					Before: flags.before,
				})
				if err != nil {
					return err
				}

				return emitBundle(cmd.OutOrStdout(), bundle, flags.raw, viewTimeline)
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&flags.repo, "repo", "",
		"the registered clone the file belongs to, by remote (github:owner/repo) or by path; omit it when only one is registered")
	f.IntVar(&flags.limit, "limit", 0, "how many commits the page holds; omit it for the server default, which the server also caps")
	f.StringVar(&flags.before, "before", "", "commit SHA to page backwards from: the page holds the commits older than it")
	f.BoolVar(&flags.raw, "raw", false, "emit the evidence bundle as JSON instead of prose")
	return cmd
}
