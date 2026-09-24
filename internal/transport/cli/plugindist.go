package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/di"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugexec"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/secrets"
	"github.com/setthasit/Lore/internal/urlx"
	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
)

const trustNotice = "installing a plugin runs that author's code on this machine, with your privileges" +
	" and the tokens you give its sources"

func newPluginInstallCommand(configPath *string, reg *registry.Registry) *cobra.Command {
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
			return runPluginInstall(cmd, args, *configPath, declaredManifest(reg.Log()))
		},
	}
}

func newPluginUpdateCommand(configPath *string, reg *registry.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "update <name>[@<version>]",
		Short: "Re-resolve a plugin and rewrite its locked version, URLs and digests",
		Long: "The only command that replaces a locked digest. Without a version it moves to\n" +
			"the newest release and writes that version back into lore.yaml.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginUpdate(cmd, args[0], *configPath, declaredManifest(reg.Log()))
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
			"reports the exact binary that will run and, when a sources: entry uses the\n" +
			"plugin, certifies it with that instance's configuration and secrets, so that\n" +
			"instance's environment variables must be exported for the suite to run. The\n" +
			"suite streams that live source for real — twice in full, then once from a\n" +
			"mid-stream cursor — and interrupting the command ends the run.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginVerify(cmd, args[0], *configPath, reg)
		},
	}
}

func runPluginInstall(cmd *cobra.Command, args []string, configPath string, handshake plugindist.Handshake) error {
	workspace, err := plugindist.Open(configPath, plugindist.WithHandshake(handshake))
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

	renderInstalls(cmd.Context(), out, configPath, results, handshake)
	return nil
}

func runPluginUpdate(cmd *cobra.Command, argument, configPath string, handshake plugindist.Handshake) error {
	workspace, err := plugindist.Open(configPath, plugindist.WithHandshake(handshake))
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

	plugin, err := plugexec.Open(cmd.Context(), report.Binary, reg.Host(name))
	if err != nil {
		return err
	}
	prepared, err := declaredPreparation(reg, workspace, name, report.Binary, plugin.Manifest())
	if err != nil {
		return err
	}
	noticesOf(cmd.Context()).print(cmd.ErrOrStderr(), reg.Sink())

	ident := prepared.instance.Ident()
	certification, err := plugexec.Certify(cmd.Context(),
		ident, plugin, reg.Host(ident), prepared.config, prepared.secrets, prepared.declared)
	if err != nil {
		return err
	}
	return renderCertification(out, reg.Sink(), prepared.instance, certification)
}

type preparedInstance struct {
	instance registry.Instance
	config   []byte
	secrets  map[string]string
	declared bool
}

func declaredPreparation(reg *registry.Registry, workspace *plugindist.Workspace, name, binary string, manifest lore.Manifest) (preparedInstance, error) {
	decl, found := workspace.SourceUsing(name)
	if !found {
		return preparedInstance{instance: registry.Instance{Use: name}}, nil
	}

	in, err := di.InstanceOf(decl, sourcesKey)
	if err != nil {
		return preparedInstance{}, err
	}
	if manifest.Kind != lore.KindSource {
		return preparedInstance{instance: registry.Instance{Use: name}}, nil
	}
	cfg, secrets, err := reg.Prepare(manifest, in, registry.OriginExternal(binary))
	if err != nil {
		return preparedInstance{}, err
	}
	return preparedInstance{instance: in, config: cfg, secrets: secrets, declared: true}, nil
}

func declaredManifest(log *slog.Logger) plugindist.Handshake {
	return func(ctx context.Context, binary string) (lore.Manifest, error) {
		plugin, err := plugexec.Open(ctx, binary, lore.Host{Log: log})
		if err != nil {
			return lore.Manifest{}, err
		}
		return plugin.Manifest(), nil
	}
}

func renderInstalls(ctx context.Context, out io.Writer, configPath string, results []plugindist.Result, handshake plugindist.Handshake) {
	workspace, openErr := plugindist.Open(configPath, plugindist.WithHandshake(handshake))
	for _, result := range results {
		renderInstall(out, result)

		manifest, err := lore.Manifest{}, openErr
		if err == nil {
			manifest, err = installedManifest(ctx, workspace, configPath, result.Name)
		}
		if err != nil {
			printfln(out, "  manifest: unreadable — %s", inertLine(internalerror.MessageOf(err)))
			continue
		}
		printfln(out, "  kind:    %s", kindLabel(manifest))
		printfln(out, "  summary: %s", inertLine(manifest.Summary))
	}
}

func installedManifest(ctx context.Context, workspace *plugindist.Workspace, configPath, name string) (lore.Manifest, error) {
	decl, declared := workspace.Declaration(name)
	if !declared {
		return lore.Manifest{}, internalerror.NewPreconditionError(
			plugindist.Label(name)+" is installed, but "+configPath+" no longer declares it", nil)
	}
	return workspace.Manifest(ctx, decl)
}

func renderInstall(out io.Writer, result plugindist.Result) {
	if result.Origin == plugindist.OriginLocal {
		printfln(out, "%s runs %s in place", plugindist.Label(result.Name), inertLine(result.Binary))
		printfln(out, "  warning: %s", inertLine(result.Warning))
		return
	}

	printfln(out, "installed %s %s for %s", plugindist.Label(result.Name), inertLine(result.Version), result.Platform)
	printfln(out, "  binary:  %s", inertLine(result.Binary))
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
			plugindist.Label(report.Name), inertLine(report.Binary))
		printfln(out, "  warning: %s", inertLine(report.Warning))
		return
	}

	printfln(out, "%s %s for %s", plugindist.Label(report.Name), inertLine(report.Version), report.Platform)
	printfln(out, "  binary:  %s", inertLine(report.Binary))
	printfln(out, "  digest:  %s (re-checked now)", report.BinaryDigest)
	printfln(out, "  locked:  %s", report.LockedDigest)
	printfln(out, "  from:    %s", inertLine(urlx.RedactIfUserinfo(report.LockedURL)))
}

func renderCertification(out io.Writer, sink *secrets.Sink, in registry.Instance, certification plugexec.Certification) error {
	label := "conformance"
	if field := in.Field; field != "" {
		label += " (" + inertLine(field) + ")"
	}

	if certification.Kind != lore.KindSource {
		printfln(out, "  %s: not run — %s is a %s plugin, and the suite certifies sources",
			label, plugindist.Label(in.Use), certification.Kind)
		return nil
	}
	if len(certification.Ran) == 0 {
		printfln(out, "  %s: not run — no check ran", label)
		renderSkipped(out, sink, certification.Skipped)
		return nil
	}

	reduced := len(certification.Skipped) > 0
	suffix := ""
	if reduced {
		suffix = fmt.Sprintf(" on %d of %d checks",
			len(certification.Ran), len(certification.Ran)+len(certification.Skipped))
	}

	var refusal error
	if len(certification.Findings) == 0 {
		printfln(out, "  %s: passed%s", label, suffix)
	} else {
		printfln(out, "  %s: %s%s", label,
			plural(len(certification.Findings), "failure", "failures"), suffix)
		for _, finding := range certification.Findings {
			printfln(out, "    %s: %s", finding.Check, inertLine(sink.Scrub(finding.Detail)))
		}
		refusal = internalerror.NewPreconditionError(plugindist.Label(in.Use)+
			" does not satisfy the plugin contract; the failures above name what a sync round would get wrong", nil)
	}

	if reduced {
		printfln(out, "    ran:")
		for _, check := range certification.Ran {
			printfln(out, "      %s", check)
		}
		renderSkipped(out, sink, certification.Skipped)
	}
	return refusal
}

func renderSkipped(out io.Writer, sink *secrets.Sink, skipped []conform.Skip) {
	printfln(out, "    skipped:")
	for _, skip := range skipped {
		printfln(out, "      %s — %s", skip.Check, inertLine(sink.Scrub(skip.Reason)))
	}
}
