package di

import (
	"fmt"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugexec"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/sdk"
)

// externals is what a workspace's `plugins:` block resolved to: the plugins
// themselves, and the warnings resolving them produced. They travel together
// because an unpinned local plugin is usable and worth warning about, and
// splitting them would let the warning be dropped on the way to the operator.
type externals struct {
	plugins  []external
	warnings registry.Warnings
}

type external struct {
	origin string
	plugin lore.Plugin
}

// newExternals resolves every declared external plugin to an installed,
// digest-checked binary and reads its manifest through the protocol handshake.
// It runs at startup, before the scheduler exists: nothing inside a sync round
// ever fetches or execs a plugin for the first time, because a background timer
// must not be able to download and run code.
func newExternals(cfg *config.Config, dir WorkspaceDir, compiled registry.Compiled) (externals, error) {
	if len(cfg.Plugins) == 0 {
		return externals{}, nil
	}

	lock, err := plugindist.LoadLock(string(dir))
	if err != nil {
		return externals{}, err
	}

	var out externals
	for _, decl := range cfg.Plugins {
		if decl.Name == "" {
			return externals{}, internalerror.NewBadRequestError(
				"every entry in plugins: must have a name — it is the token `use:` refers to", nil)
		}
		// A declaration that shadows a compiled plugin would make `use:` ambiguous,
		// and which one won would depend on registration order.
		if _, taken := compiled.Manifest(decl.Name); taken {
			return externals{}, internalerror.NewBadRequestError(fmt.Sprintf(
				"plugins[%s] names a plugin this build already compiles in; pick another name or drop the declaration",
				decl.Name), nil)
		}

		coord, err := plugindist.Resolve(string(dir), decl)
		if err != nil {
			return externals{}, err
		}

		binary, err := plugindist.Binary(decl.Name, coord, lock)
		if err != nil {
			return externals{}, err
		}

		// The handshake failure is classified here rather than left raw: it
		// arrives from a subprocess through the wiring graph, and an unclassified
		// error would reach the operator wrapped in constructor plumbing instead
		// of naming the plugin they have to fix.
		plugin, err := plugexec.Open(binary, compiled.Host(decl.Name))
		if err != nil {
			return externals{}, internalerror.NewPreconditionError(fmt.Sprintf(
				"plugins[%s] does not answer the plugin protocol at %s: %s", decl.Name, binary, err.Error()), err)
		}

		out.plugins = append(out.plugins, external{origin: "external " + binary, plugin: plugin})
		if warning := coord.Warning(); warning != "" {
			out.warnings = append(out.warnings, warning)
		}
	}
	return out, nil
}

// newWorkspaceRegistry is the registry every instance in this workspace is
// built from: the compiled set plus whatever `plugins:` declared, on a clone so
// the compiled set stays exactly what the composition root passed.
func newWorkspaceRegistry(compiled registry.Compiled, ext externals) (*registry.Registry, error) {
	reg := compiled.Clone()
	for _, e := range ext.plugins {
		if err := reg.RegisterExternal(e.origin, e.plugin); err != nil {
			return nil, err
		}
	}
	return reg, nil
}
