package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.uber.org/fx"

	"lore/internal/di"
	"lore/internal/services"
)

type Runtime struct {
	Query  services.QueryService
	Sync   services.SyncOrchestrator
	Status services.StatusService
}

type Resolver func(ctx context.Context, configPath string) (*Runtime, func() error, error)

func resolveWithFX(ctx context.Context, configPath string) (*Runtime, func() error, error) {
	rt := new(Runtime)

	app := fx.New(
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

	// Stop on a cancelled context would skip the hooks that close the index.
	return rt, func() error { return app.Stop(context.WithoutCancel(ctx)) }, nil
}

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
