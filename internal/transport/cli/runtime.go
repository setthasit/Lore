package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.uber.org/fx"

	"lore/internal/config"
	"lore/internal/di"
	"lore/internal/services"
)

type Runtime struct {
	Config  *config.Config
	Query   services.QueryService
	Why     services.WhyService
	Trace   services.TraceService
	Impact  services.ImpactService
	History services.HistoryService
	Sync    services.SyncOrchestrator
	Status  services.StatusService
}

type Resolver func(ctx context.Context, configPath string) (*Runtime, func() error, error)

func resolveWithFX(ctx context.Context, configPath string) (*Runtime, func() error, error) {
	rt := new(Runtime)

	app := fx.New(
		fx.NopLogger,
		di.Workspace(configPath),
		fx.Populate(&rt.Config, &rt.Query, &rt.Why, &rt.Trace, &rt.Impact, &rt.History, &rt.Sync, &rt.Status),
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
	for _, warning := range rt.Config.StartupWarnings() {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "lore: warning: "+warning)
	}

	if err := run(rt); err != nil {
		_ = stop()
		return err
	}
	return stop()
}
