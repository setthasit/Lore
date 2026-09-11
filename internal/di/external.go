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

type externals struct {
	plugins  []external
	warnings registry.Warnings
}

type external struct {
	name   string
	origin string
	plugin lore.Plugin
}

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
		coord, err := plugindist.Resolve(string(dir), decl)
		if err != nil {
			return externals{}, err
		}

		binary, err := plugindist.Binary(coord, lock)
		if err != nil {
			return externals{}, err
		}

		plugin, err := plugexec.Open(binary, compiled.Host(decl.Name))
		if err != nil {
			return externals{}, internalerror.NewPreconditionError(fmt.Sprintf(
				"plugins[%s] does not answer the plugin protocol at %s: %s", decl.Name, binary, err.Error()), err)
		}

		out.plugins = append(out.plugins, external{name: decl.Name, origin: registry.OriginExternal(binary), plugin: plugin})
		if warning := coord.Warning(); warning != "" {
			out.warnings = append(out.warnings, warning)
		}
	}
	return out, nil
}

func newWorkspaceRegistry(compiled registry.Compiled, ext externals) (*registry.Registry, error) {
	reg := compiled.Clone()
	for _, e := range ext.plugins {
		if err := reg.RegisterExternal(e.origin, e.name, e.plugin); err != nil {
			return nil, err
		}
	}
	return reg, nil
}
