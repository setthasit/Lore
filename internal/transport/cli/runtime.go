package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.uber.org/fx"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/di"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport"
)

type Runtime struct {
	Config *config.Config

	// Warnings are configuration facts that degrade answers without being errors.
	Warnings  registry.Warnings
	Query     services.QueryService
	Why       services.WhyService
	Trace     services.TraceService
	Impact    services.ImpactService
	History   services.HistoryService
	Sync      services.SyncOrchestrator
	Status    services.StatusService
	Synthesis services.SynthesisService
}

// modules are fx options beyond the workspace: only `lore serve` adds the scheduler.
type Resolver func(ctx context.Context, configPath string, modules ...fx.Option) (*Runtime, func() error, error)

// fxResolver closes over the registry so every command resolves a workspace
// against the same plugin set the binary was assembled from.
func fxResolver(reg *registry.Registry) Resolver {
	return func(ctx context.Context, configPath string, modules ...fx.Option) (*Runtime, func() error, error) {
		return resolveWithFX(ctx, reg, configPath, modules...)
	}
}

func resolveWithFX(
	ctx context.Context,
	reg *registry.Registry,
	configPath string,
	modules ...fx.Option,
) (*Runtime, func() error, error) {
	rt := new(Runtime)

	app := fx.New(
		append([]fx.Option{
			fx.NopLogger,
			di.Workspace(configPath, reg),
			fx.Populate(&rt.Config, &rt.Warnings, &rt.Query, &rt.Why, &rt.Trace, &rt.Impact, &rt.History,
				&rt.Sync, &rt.Status, &rt.Synthesis),
		}, modules...)...,
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

func (rt *Runtime) services() transport.Services {
	return transport.Services{
		Query:   rt.Query,
		Why:     rt.Why,
		Trace:   rt.Trace,
		Impact:  rt.Impact,
		History: rt.History,
		Sync:    rt.Sync,
		Status:  rt.Status,
	}
}

func withRuntime(
	cmd *cobra.Command,
	resolve Resolver,
	configPath string,
	run func(*Runtime) error,
	modules ...fx.Option,
) error {
	rt, stop, err := resolve(cmd.Context(), configPath, modules...)
	if err != nil {
		return err
	}
	for _, warning := range rt.Warnings {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "lore: warning: "+warning)
	}

	if err := run(rt); err != nil {
		_ = stop()
		return err
	}
	return stop()
}
