package app

import (
	"os"

	"github.com/setthasit/Lore/internal/di"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/transport/cli"
	"github.com/setthasit/Lore/sdk"
)

func Run(plugins ...lore.Plugin) int {
	reg := registry.New(Host())
	if err := reg.Register(plugins...); err != nil {
		return cli.Report(os.Stderr, err)
	}
	return cli.Main(reg)
}

func Host() lore.Host {
	return lore.Host{Log: di.DiagnosticLogger()}
}
