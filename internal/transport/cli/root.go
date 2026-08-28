package cli

import "github.com/spf13/cobra"

func newRootCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "lore",
		Short:         "Provenance and decision archaeology for your codebase",
		Long:          "Lore indexes the decision trail across your sources and answers why decisions were made and what happened next, with every claim carrying a source URL.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
}

// Execute runs the lore root command.
func Execute() error {
	return newRootCommand().Execute()
}
