package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"lore/internal/errors/internalerror"
)

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

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return internalerror.NewBadRequestError(err.Error(), nil)
	})

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

func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return internalerror.NewBadRequestError(err.Error(), nil)
		}
		return nil
	}
}

// An interrupt cancels the command's context rather than killing the process.
func Main() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCommand(resolveWithFX).ExecuteContext(ctx); err != nil {
		return report(os.Stderr, err)
	}
	return exitOK
}
