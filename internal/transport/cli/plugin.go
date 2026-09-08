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
			externals, err := declaredExternals(*configPath, reg)
			if err != nil {
				return err
			}
			renderPlugins(cmd.OutOrStdout(), reg.List(), externals)
			return nil
		},
	}
}

// Its manifest is not read: reading one means executing the binary, and listing must not do that.
type externalRow struct {
	name  string
	from  string
	state string
}

func declaredExternals(configPath string, reg *registry.Registry) ([]externalRow, error) {
	workspace, err := plugindist.Open(configPath)
	switch {
	case internalerror.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}

	var rows []externalRow
	for _, decl := range workspace.Plugins() {
		if _, compiled := reg.Manifest(decl.Name); compiled {
			continue
		}

		row := externalRow{name: decl.Name, from: urlx.RedactIfUserinfo(decl.From)}
		switch install, err := workspace.Installed(decl); {
		case err != nil:
			row.state = "unresolvable — " + internalerror.MessageOf(err)
		case install.Fault != nil:
			row.state = internalerror.MessageOf(install.Fault)
		case install.Binary == "":
			row.state = "not installed — run: lore plugin install " + decl.Name
		default:
			row.state = registry.OriginExternal(install.Binary)
		}
		rows = append(rows, row)
	}
	return rows, nil
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
