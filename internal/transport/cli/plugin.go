package cli

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
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
		newPluginSearchCommand(),
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
			renderPlugins(cmd, reg.List(), declaredExternals(*configPath, reg))
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
	workspace, err := openPluginWorkspace(configPath)
	if err != nil {
		return nil
	}

	var rows []externalRow
	for _, decl := range workspace.config.Plugins {
		if _, compiled := reg.Manifest(decl.Name); compiled {
			continue
		}

		row := externalRow{name: decl.Name, from: decl.From}
		switch coord, err := plugindist.Resolve(workspace.dir, decl); {
		case err != nil:
			row.state = "unresolvable — " + actionableMessage(err)
		default:
			binary, err := workspace.store.Binary(decl.Name, coord, workspace.lock)
			if err != nil {
				row.state = "not installed — run: lore plugin install " + decl.Name
			} else {
				row.state = "external " + binary
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// The columns are padded rather than tabwritten because the set is small and a
// fixed-width table stays readable when a summary wraps.
func renderPlugins(cmd *cobra.Command, entries []registry.Entry, externals []externalRow) {
	out := cmd.OutOrStdout()
	if len(entries) == 0 {
		printfln(out, "no plugins are registered — this build can ingest nothing")
		return
	}

	nameWidth, kindWidth, originWidth := len("NAME"), len("KIND"), len("ORIGIN")
	for _, e := range entries {
		nameWidth = max(nameWidth, len(e.Manifest.Name))
		kindWidth = max(kindWidth, len(kindLabel(e.Manifest)))
		originWidth = max(originWidth, len(e.Origin))
	}

	printfln(out, "%s  %s  %s  %s",
		pad("NAME", nameWidth), pad("KIND", kindWidth), pad("ORIGIN", originWidth), "SUMMARY")
	for _, e := range entries {
		printfln(out, "%s  %s  %s  %s",
			pad(e.Manifest.Name, nameWidth),
			pad(kindLabel(e.Manifest), kindWidth),
			pad(e.Origin, originWidth),
			e.Manifest.Summary)
	}

	// Declared externals print below the table rather than inside it: their
	// kind and summary live in a manifest only the binary can answer for, and
	// listing does not execute anything.
	for _, row := range externals {
		printfln(out, "")
		printfln(out, "%s  declared from %s", pad(row.name, nameWidth), row.from)
		printfln(out, "%s  %s", pad("", nameWidth), row.state)
	}
}

// A provider's kind alone says nothing useful, so the capabilities it serves are
// what the column reports: that is the part a role binding has to match.
func kindLabel(m lore.Manifest) string {
	if m.Kind != lore.KindProvider {
		return string(m.Kind)
	}

	served := make([]string, 0, 2)
	for _, c := range m.Capabilities.Names() {
		served = append(served, string(c))
	}
	if len(served) == 0 {
		return string(m.Kind)
	}
	return string(m.Kind) + " (" + strings.Join(served, ", ") + ")"
}

func pad(text string, width int) string {
	if n := width - len(text); n > 0 {
		return text + strings.Repeat(" ", n)
	}
	return text
}

// pluralize keeps the failure lines readable without a second format string.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + singular
	}
	return strconv.Itoa(n) + " " + plural
}
