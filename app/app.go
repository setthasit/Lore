package app

import (
	"log/slog"
	"os"

	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/secrets"
	"github.com/setthasit/Lore/internal/transport/cli"
	"github.com/setthasit/Lore/sdk"
)

func Run(plugins ...lore.Plugin) int {
	sink := &secrets.Sink{}
	// Stdout carries the MCP JSON-RPC stream, so diagnostics belong on stderr.
	log := slog.New(slog.NewTextHandler(sink.Writer(os.Stderr), nil))
	reg := registry.New(lore.Host{Log: log}, sink)
	if err := reg.Register(plugins...); err != nil {
		return cli.Report(os.Stderr, err)
	}
	return cli.Main(reg)
}
