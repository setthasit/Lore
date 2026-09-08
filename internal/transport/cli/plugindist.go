package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/di"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugexec"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/urlx"
	"github.com/setthasit/Lore/sdk"
)

const trustNotice = "installing a plugin runs that author's code on this machine, with your privileges" +
	" and the tokens you give its sources"

func newPluginInstallCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "install [<name> | <coordinate>[@latest]]",
		Short: "Resolve, download, verify and pin an external plugin",
		Long: "Resolves a plugin's coordinate, downloads and verifies the artifact for this\n" +
			"os/arch, unpacks it under ~/.lore/plugins and records the version, URL and\n" +
			"digest in lore.lock. With no argument it installs every plugin lore.yaml\n" +
			"declares. Nothing else in Lore ever downloads a plugin: a sync round does\n" +
			"not, and a scheduler must not.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginInstall(cmd, args, *configPath)
		},
	}
}

func newPluginUpdateCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "update <name>[@<version>]",
		Short: "Re-resolve a plugin and rewrite its locked version, URLs and digests",
		Long: "The only command that replaces a locked digest. Without a version it moves to\n" +
			"the newest release and writes that version back into lore.yaml.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginUpdate(cmd, args[0], *configPath)
		},
	}
}

func newPluginRemoveCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Drop a plugin's declaration, its lock entry and its cached versions",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginRemove(cmd, args[0], *configPath)
		},
	}
}

func newPluginVerifyCommand(configPath *string, reg *registry.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "verify <name>",
		Short: "Re-check the digest of an installed plugin and report it",
		Long: "Re-hashes the installed binary and compares it with the digest recorded when it\n" +
			"was installed, so a cached binary rewritten after installation is caught. It\n" +
			"reports the exact binary that will run.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginVerify(cmd, args[0], *configPath, reg)
		},
	}
}

func runPluginInstall(cmd *cobra.Command, args []string, configPath string) error {
	workspace, err := plugindist.Open(configPath, plugindist.WithHandshake(declaredManifest))
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	results, err := workspace.Install(cmd.Context(), args, func() { printfln(out, "%s", trustNotice) })
	if err != nil {
		return err
	}
	if len(results) == 0 {
		printfln(out, "%s declares no plugins: — nothing to install", configPath)
		return nil
	}

	for _, result := range results {
		renderInstall(out, result)
	}
	return nil
}

func runPluginUpdate(cmd *cobra.Command, argument, configPath string) error {
	workspace, err := plugindist.Open(configPath, plugindist.WithHandshake(declaredManifest))
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	result, err := workspace.Update(cmd.Context(), argument, func() { printfln(out, "%s", trustNotice) })
	if err != nil {
		return err
	}
	renderInstall(out, result)
	return nil
}

func runPluginRemove(cmd *cobra.Command, name, configPath string) error {
	workspace, err := plugindist.Open(configPath)
	if err != nil {
		return err
	}
	removal, err := workspace.Remove(name)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	printfln(out, "removed %s from %s", plugindist.Label(name), configPath)
	if removal.Unlocked {
		printfln(out, "  dropped its entry from %s", plugindist.LockFileName)
	}
	printfln(out, "  deleted %s from the plugin cache", plural(removal.Versions, "cached version", "cached versions"))
	return nil
}

func runPluginVerify(cmd *cobra.Command, name, configPath string, reg *registry.Registry) error {
	workspace, err := plugindist.Open(configPath)
	if err != nil {
		return err
	}
	report, err := workspace.Verify(name)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	renderVerify(out, report)

	certification, err := plugexec.Certify(name, report.Binary, reg.Host(name))
	if err != nil {
		return err
	}
	return renderCertification(out, name, certification)
}

func declaredManifest(binary string) (lore.Manifest, error) {
	plugin, err := plugexec.Open(binary, lore.Host{Log: di.DiagnosticLogger()})
	if err != nil {
		return lore.Manifest{}, err
	}
	return plugin.Manifest(), nil
}

func renderInstall(out io.Writer, result plugindist.Result) {
	if result.Origin == plugindist.OriginLocal {
		printfln(out, "%s runs %s in place", plugindist.Label(result.Name), result.Binary)
		printfln(out, "  warning: %s", result.Warning)
		return
	}

	printfln(out, "installed %s %s for %s", plugindist.Label(result.Name), result.Version, result.Platform)
	printfln(out, "  binary:  %s", result.Binary)
	printfln(out, "  digest:  %s", result.LockedDigest)
	switch {
	case result.Locked:
		printfln(out, "  matches the digest %s already pinned for %s", plugindist.LockFileName, result.Platform)
	case result.Trust:
		printfln(out, "  pinned in %s: nothing but the download itself vouched for these bytes", plugindist.LockFileName)
	default:
		printfln(out, "  pinned in %s", plugindist.LockFileName)
	}
	if result.Signed {
		printfln(out, "  signature: verified")
	}
}

func renderVerify(out io.Writer, report plugindist.Report) {
	if report.Origin == plugindist.OriginLocal {
		printfln(out, "%s runs %s in place — unpinned, unlocked, undigested",
			plugindist.Label(report.Name), report.Binary)
		printfln(out, "  warning: %s", report.Warning)
		return
	}

	printfln(out, "%s %s for %s", plugindist.Label(report.Name), report.Version, report.Platform)
	printfln(out, "  binary:  %s", report.Binary)
	printfln(out, "  digest:  %s (re-checked now)", report.BinaryDigest)
	printfln(out, "  locked:  %s", report.LockedDigest)
	printfln(out, "  from:    %s", urlx.RedactIfUserinfo(report.LockedURL))
}

func renderCertification(out io.Writer, name string, certification plugexec.Certification) error {
	if certification.Kind != lore.KindSource {
		printfln(out, "  conformance: not run — %s is a %s plugin, and the suite certifies sources",
			name, certification.Kind)
		return nil
	}
	if len(certification.Findings) == 0 {
		printfln(out, "  conformance: passed")
		return nil
	}

	printfln(out, "  conformance: %s", plural(len(certification.Findings), "failure", "failures"))
	for _, finding := range certification.Findings {
		printfln(out, "    %s: %s", finding.Check, finding.Detail)
	}
	return internalerror.NewPreconditionError(
		name+" does not satisfy the plugin contract; the failures above name what a sync round would get wrong", nil)
}
