package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/registry"
)

const defaultConfigPath = "./lore.yaml"

func newRootCommand(resolve Resolver, reg *registry.Registry) *cobra.Command {
	var (
		configPath  = new(string)
		showVersion = new(bool)
	)

	root := &cobra.Command{
		Use:           "lore",
		Short:         "Provenance and decision archaeology for your codebase",
		Long:          "Lore indexes the decision trail across your sources and answers why decisions were made and what happened next, with every claim carrying a source URL.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if *showVersion {
				return runVersion(cmd, resolve, *configPath)
			}
			return cmd.Help()
		},
	}

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return internalerror.NewBadRequestError(err.Error(), nil)
	})

	root.PersistentFlags().StringVar(configPath, "config", defaultConfigPath, "path to lore.yaml")
	root.Flags().BoolVar(showVersion, "version", false,
		"print the build stamp and the workspace's embedder identity")

	root.AddCommand(
		newInitCommand(configPath, reg),
		newSourceCommand(configPath, reg),
		newPluginCommand(configPath, reg),
		newBuildCommand(),
		newSyncCommand(resolve, configPath),
		newStatusCommand(resolve, configPath),
		newAskCommand(resolve, configPath),
		newWhyCommand(resolve, configPath),
		newTraceCommand(resolve, configPath),
		newImpactCommand(resolve, configPath),
		newHistoryCommand(resolve, configPath),
		newMCPCommand(resolve, configPath),
		newServeCommand(resolve, configPath),
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
func Main(reg *registry.Registry) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCommand(fxResolver(reg), reg).ExecuteContext(ctx); err != nil {
		return report(os.Stderr, err)
	}
	return exitOK
}
