package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"lore/internal/errors/internalerror"
)

// defaultConfigPath is where every command looks for the workspace
// configuration unless told otherwise.
const defaultConfigPath = "./lore.yaml"

func newRootCommand(resolve Resolver) *cobra.Command {
	root := &cobra.Command{
		Use:           "lore",
		Short:         "Provenance and decision archaeology for your codebase",
		Long:          "Lore indexes the decision trail across your sources and answers why decisions were made and what happened next, with every claim carrying a source URL.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	// A malformed invocation is the caller's mistake like any other, so cobra's
	// own complaints are classified rather than falling through as unclassified
	// internal failures: exit 2 has to mean "you typed it wrong" for a bad flag
	// as much as for a bad --since.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return internalerror.NewBadRequestError(err.Error(), nil)
	})

	// The path is shared by pointer rather than re-read per command: it is one
	// value the root owns, and subcommands are built before it is parsed.
	configPath := new(string)
	root.PersistentFlags().StringVar(configPath, "config", defaultConfigPath, "path to lore.yaml")

	root.AddCommand(
		newInitCommand(configPath),
		newSyncCommand(resolve, configPath),
		newStatusCommand(resolve, configPath),
		newAskCommand(resolve, configPath),
		newMCPCommand(resolve, configPath),
	)
	return root
}

// usageArgs classifies a positional-argument complaint the same way
// SetFlagErrorFunc classifies a flag one. Cobra's validators are kept as they
// are: the wrapper only labels what they report.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return internalerror.NewBadRequestError(err.Error(), nil)
		}
		return nil
	}
}

// Main runs the lore command line and returns the process exit code.
//
// Interrupts cancel the command's context rather than killing the process, so a
// long-running surface — a sync round mid-batch, `lore mcp` serving a client —
// stops at its own checkpoint and still closes the index.
func Main() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCommand(resolveWithFX).ExecuteContext(ctx); err != nil {
		return report(os.Stderr, err)
	}
	return exitOK
}
