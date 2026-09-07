package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugbuild"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/urlx"
	"github.com/setthasit/Lore/sdk"
)

func newPluginCommand(configPath *string, reg *registry.Registry) *cobra.Command {
	plugin := &cobra.Command{
		Use:   "plugin",
		Short: "Inspect, install and verify the plugins this build can use",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	plugin.AddCommand(
		newPluginListCommand(configPath, reg),
		newPluginInstallCommand(configPath),
		newPluginUpdateCommand(configPath),
		newPluginRemoveCommand(configPath),
		newPluginVerifyCommand(configPath, reg),
		newPluginSearchCommand(plugbuild.Index{}),
	)
	return plugin
}

func newPluginListCommand(configPath *string, reg *registry.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every plugin this build can use",
		Long: "Lists what this binary can be configured with: an official plugin holds no\n" +
			"privilege a third-party one lacks, so both appear here the same way. A\n" +
			"plugin declared under plugins: but not installed is listed too, so the gap\n" +
			"between what the configuration asks for and what is on disk is visible.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			renderPlugins(cmd.OutOrStdout(), reg.List(), declaredExternals(*configPath, reg))
			return nil
		},
	}
}

// externalRow is a declared external plugin. Its manifest is deliberately not
// read: reading one means executing the binary, and listing must not do that.
type externalRow struct {
	name  string
	from  string
	state string
}

// declaredExternals reports the `plugins:` entries this build would resolve at
// startup. It is best-effort on purpose: `lore plugin list` answers what a
// binary can be configured with, and refusing to answer because a workspace is
// half-configured would withhold the information most likely to explain why.
func declaredExternals(configPath string, reg *registry.Registry) []externalRow {
	workspace, err := plugindist.Open(configPath)
	if err != nil {
		return nil
	}

	var rows []externalRow
	for _, decl := range workspace.Plugins() {
		if _, compiled := reg.Manifest(decl.Name); compiled {
			continue
		}

		row := externalRow{name: decl.Name, from: urlx.RedactIfUserinfo(decl.From)}
		switch binary, err := workspace.Installed(decl); {
		case err != nil:
			row.state = "unresolvable — " + internalerror.MessageOf(err)
		case binary == "":
			row.state = "not installed — run: lore plugin install " + decl.Name
		default:
			row.state = registry.OriginExternal(binary)
		}
		rows = append(rows, row)
	}
	return rows
}

func renderPlugins(out io.Writer, entries []registry.Entry, externals []externalRow) {
	if len(entries) == 0 {
		printfln(out, "no plugins are registered — this build can ingest nothing")
		return
	}

	header := []string{"NAME", "KIND", "ORIGIN", "SUMMARY"}
	rows := make([][]string, len(entries))
	for i, e := range entries {
		rows[i] = []string{e.Manifest.Name, kindLabel(e.Manifest), e.Origin, e.Manifest.Summary}
	}
	widths := renderTable(out, header, rows)

	for _, row := range externals {
		printfln(out, "")
		printfln(out, "%s", tableLine([]string{row.name, "declared from " + row.from}, widths))
		printfln(out, "%s", tableLine([]string{"", row.state}, widths))
	}
}

func kindLabel(m lore.Manifest) string {
	if m.Kind != lore.KindProvider || len(m.Capabilities.Names()) == 0 {
		return string(m.Kind)
	}
	return string(m.Kind) + " (" + m.Capabilities.String() + ")"
}
