package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/plugbuild"
)

func newBuildCommand() *cobra.Command {
	var (
		with   []string
		output string
	)

	build := &cobra.Command{
		Use:   "build --with <module>@<version> [-o lore]",
		Short: "Build a lore binary with third-party plugins compiled in",
		Long: "Generates the composition root this binary was built from, with the named plugin\n" +
			"modules added to the official set, and compiles it. Go cannot load code\n" +
			"dynamically, so a compiled third-party plugin always means a new binary.\n\n" +
			"It needs a Go toolchain: that is the whole trade for in-process calls and\n" +
			"compile-time type safety. A plugin run out of process needs none — see\n" +
			"`lore plugin install`.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBuild(cmd, with, output)
		},
	}

	build.Flags().StringSliceVar(&with, "with", nil,
		"plugin module to compile in: github.com/owner/repo@v1.2.3[=package]; repeatable and comma-separated")
	build.Flags().StringVarP(&output, "output", "o", plugbuild.DefaultOutput, "path of the binary to write")
	return build
}

func runBuild(cmd *cobra.Command, with []string, output string) error {
	coordinates := make([]plugbuild.Coordinate, 0, len(with))
	for _, raw := range with {
		coordinate, err := plugbuild.ParseCoordinate(raw)
		if err != nil {
			return err
		}
		coordinates = append(coordinates, coordinate)
	}

	// Progress goes to stderr so a script piping stdout gets only the report.
	result, err := plugbuild.Build(cmd.Context(), plugbuild.Request{
		Coordinates: coordinates,
		Output:      output,
		Progress:    cmd.ErrOrStderr(),
	})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	printfln(out, "wrote %s against engine %s with %s compiled in:",
		result.Output, result.Engine, plural(len(result.Added), "plugin", "plugins"))
	for _, added := range result.Added {
		printfln(out, "  %s", added)
	}
	printfln(out, "\nthe binary reports:")
	_, _ = io.WriteString(out, result.Plugins)
	return nil
}

func newPluginSearchCommand(index plugbuild.Index) *cobra.Command {
	return &cobra.Command{
		Use:   "search <query>",
		Short: "Search the plugin index",
		Long: "Matches the query against each indexed plugin's name, summary and kind. The index\n" +
			"is a JSON file in a git repository, read fresh every time and never cached to\n" +
			"disk; there is no ranking, so results keep the index's own order.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginSearch(cmd, index, args[0])
		},
	}
}

func runPluginSearch(cmd *cobra.Command, index plugbuild.Index, query string) error {
	entries, skipped, err := index.Fetch(cmd.Context())
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if skipped > 0 {
		printfln(out, "left out %s the index publishes but this build refuses to show: an unprintable, over-long or "+
			"malformed entry can disguise the coordinate you would copy from it", plural(skipped, "entry", "entries"))
		if len(entries) == 0 {
			return nil
		}
	}
	if len(entries) == 0 {
		printfln(out, "the plugin index is empty — nothing is published yet; `lore plugin list` shows what this build already has")
		return nil
	}

	matched := plugbuild.Match(entries, query)
	if len(matched) == 0 {
		printfln(out, "no plugin matches %q — the index holds %s, searched by name, summary and kind",
			query, plural(len(entries), "plugin", "plugins"))
		return nil
	}
	renderSearchResults(out, matched)
	return nil
}

func renderSearchResults(out io.Writer, entries []plugbuild.Entry) {
	rows := make([][]string, len(entries))
	for i, e := range entries {
		rows[i] = []string{e.Name, e.Kind, e.Coordinate, e.Summary}
	}
	renderTable(out, []string{"NAME", "KIND", "COORDINATE", "SUMMARY"}, rows)
}
