package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.uber.org/fx"

	"lore/internal/di"
	"lore/internal/services"
)

// Runtime is the wired service set a command operates on. Nothing beyond it is
// reachable from here: a transport calls services and only services
// (02 — Layering).
type Runtime struct {
	Query  services.QueryService
	Sync   services.SyncOrchestrator
	Status services.StatusService
}

// Resolver builds the runtime for the workspace configured at configPath and
// returns the function that shuts it down.
//
// Commands hold a Resolver rather than a Runtime because building one reads
// lore.yaml and opens the index: `lore init`, `--help` and a mistyped subcommand
// must all work in a directory that has neither. Tests supply a Resolver over
// doubles and never build an fx application.
type Resolver func(ctx context.Context, configPath string) (*Runtime, func() error, error)

// resolveWithFX is the production Resolver: it builds the workspace graph and
// hands back its shutdown.
func resolveWithFX(ctx context.Context, configPath string) (*Runtime, func() error, error) {
	rt := new(Runtime)

	app := fx.New(
		// The CLI owns stdout and stderr — `lore mcp` speaks a protocol over
		// them — so fx logs nowhere.
		fx.NopLogger,
		di.Workspace(configPath),
		fx.Populate(&rt.Query, &rt.Sync, &rt.Status),
	)
	if err := app.Err(); err != nil {
		return nil, nil, err
	}
	if err := app.Start(ctx); err != nil {
		return nil, nil, err
	}

	// Shutdown must outlive a cancelled context: an interrupted command still
	// has to close the index, and Stop on a cancelled context would skip the
	// hooks that do it.
	return rt, func() error { return app.Stop(context.WithoutCancel(ctx)) }, nil
}

// withRuntime resolves the workspace, hands it to run, and shuts it down again.
// A shutdown failure is reported only when the command itself succeeded: the
// reason the command failed is the more useful of the two.
func withRuntime(cmd *cobra.Command, resolve Resolver, configPath string, run func(*Runtime) error) error {
	rt, stop, err := resolve(cmd.Context(), configPath)
	if err != nil {
		return err
	}
	if err := run(rt); err != nil {
		_ = stop()
		return err
	}
	return stop()
}
