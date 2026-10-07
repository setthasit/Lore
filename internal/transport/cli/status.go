package cli

import (
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/entities"
)

func newStatusCommand(resolve Resolver, configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report index counts, source and clone states and the sync lock",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				stats, err := rt.Status.Status(cmd.Context())
				if err != nil {
					return err
				}
				renderStatus(cmd.OutOrStdout(), stats, time.Now())
				return nil
			})
		},
	}
}

func renderStatus(w io.Writer, stats entities.IndexStats, now time.Time) {
	printfln(w, "documents: %d", stats.Documents)
	printfln(w, "chunks:    %d", stats.Chunks)
	printfln(w, "edges:     %d", stats.Edges)

	printfln(w, "")
	if len(stats.Sources) == 0 {
		printfln(w, "sources: none configured or indexed")
	} else {
		printfln(w, "sources:")
		for _, source := range stats.Sources {
			suffix := ""
			if !source.Configured {
				suffix = " (not configured)"
			}
			if source.LastCheckpoint.IsZero() {
				printfln(w, "  %-10s %d docs, never synced%s", inertLine(source.ID), source.Documents, suffix)
				continue
			}
			printfln(w, "  %-10s %d docs, last checkpoint %s (%s)%s",
				inertLine(source.ID), source.Documents, humanizeAge(now.Sub(source.LastCheckpoint)),
				source.LastCheckpoint.UTC().Format(time.RFC3339), suffix)
		}
	}

	printfln(w, "")
	if len(stats.Clones) == 0 {
		printfln(w, "clones: none registered")
	} else {
		printfln(w, "clones:")
		for _, clone := range stats.Clones {
			state := "not synced by any source"
			if clone.Synced {
				state = "synced"
			}
			printfln(w, "  %s %s", inertLine(clone.Name), state)
		}
	}

	printfln(w, "")
	if stats.Lease == nil {
		printfln(w, "sync lock: free")
		return
	}
	printfln(w, "sync lock: held by %s since %s, heartbeat %s",
		inertLine(stats.Lease.Holder),
		stats.Lease.AcquiredAt.UTC().Format(time.RFC3339),
		humanizeAge(now.Sub(stats.Lease.HeartbeatAt)))
}
