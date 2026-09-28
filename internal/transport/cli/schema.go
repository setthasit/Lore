package cli

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/configschema"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/fsx"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
)

const schemaSuffix = ".schema.json"

func newSchemaCommand(configPath *string, reg *registry.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Write the JSON Schema editors use to complete and check lore.yaml",
		Long: "Writes <config>" + schemaSuffix + " beside the configuration, generated from the\n" +
			"manifests of the plugins this build registers and of the installed plugins\n" +
			"lore.yaml declares. `lore init` writes it once and points the scaffold's first\n" +
			"line at it, which yaml-language-server reads; rerun this command after\n" +
			"installing, updating or removing a plugin. The schema checks keys and types;\n" +
			"the environment, ids and plugin rules are still checked when Lore loads the file.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSchema(cmd, *configPath, reg)
		},
	}
}

func runSchema(cmd *cobra.Command, configPath string, reg *registry.Registry) error {
	catalog, unread, err := workspaceCatalog(cmd.Context(), configPath, reg)
	if err != nil {
		return err
	}
	path, err := writeSchema(configPath, catalog)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	printfln(out, "wrote %s", path)
	for _, refusal := range unread {
		printfln(out, "  unchecked: %s", inertLine(refusal))
	}
	return nil
}

// unread explains, per declared plugin, why its with: blocks go unchecked.
func workspaceCatalog(ctx context.Context, configPath string, reg *registry.Registry) (
	catalog configschema.Catalog, unread []string, err error) {
	catalog.Plugins = reg.List()

	workspace, err := plugindist.Open(configPath, plugindist.WithHandshake(declaredManifest(reg.Log())))
	switch {
	case internalerror.IsNotFound(err):
		return catalog, nil, nil
	case err != nil:
		return configschema.Catalog{}, nil, err
	}

	for _, decl := range workspace.Plugins() {
		if _, compiled := reg.Manifest(decl.Name); compiled {
			continue
		}
		entry, err := installedEntry(ctx, workspace, decl)
		if err != nil {
			catalog.Unread = append(catalog.Unread, decl.Name)
			unread = append(unread, plugindist.Label(decl.Name)+" — "+internalerror.MessageOf(err))
			continue
		}
		catalog.Plugins = append(catalog.Plugins, entry)
	}
	return catalog, unread, nil
}

func installedEntry(ctx context.Context, workspace *plugindist.Workspace, decl config.PluginDecl) (registry.Entry, error) {
	install, err := workspace.Installed(decl)
	switch {
	case err != nil:
		return registry.Entry{}, err
	case install.Fault != nil:
		return registry.Entry{}, install.Fault
	case install.Binary == "":
		return registry.Entry{}, internalerror.NewPreconditionError("not installed — run: lore plugin install "+decl.Name, nil)
	}

	manifest, err := installedExternal(ctx, workspace, decl.Name)
	if err != nil {
		return registry.Entry{}, err
	}
	return registry.Entry{Manifest: manifest, Origin: registry.OriginExternal(install.Binary)}, nil
}

func writeSchema(configPath string, catalog configschema.Catalog) (string, error) {
	body, err := configschema.Generate(catalog)
	if err != nil {
		return "", err
	}
	path := schemaPath(configPath)
	if err := fsx.WriteAtomic(path, body, 0o644); err != nil {
		return "", internalerror.NewInternalError("cannot write "+path, err)
	}
	return path, nil
}

func schemaPath(configPath string) string {
	return strings.TrimSuffix(configPath, filepath.Ext(configPath)) + schemaSuffix
}

func schemaModeline(configPath string) string {
	return "# yaml-language-server: $schema=./" + filepath.Base(schemaPath(configPath)) + "\n"
}
