package cli

import (
	"io"
	"time"

	"github.com/spf13/cobra"

	"lore/internal/entities"
)

func newStatusCommand(resolve Resolver, configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report index counts, per-source cursor ages and the sync lock",
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
	if len(stats.Cursors) == 0 {
		printfln(w, "sources: none have checkpointed yet — run `lore sync`")
	} else {
		printfln(w, "sources:")
		for _, c := range stats.Cursors {
			printfln(w, "  %-10s last checkpoint %s (%s)",
				c.Connector, humanizeAge(now.Sub(c.UpdatedAt)), c.UpdatedAt.UTC().Format(time.RFC3339))
		}
	}

	printfln(w, "")
	if stats.Lease == nil {
		printfln(w, "sync lock: free")
		return
	}
	printfln(w, "sync lock: held by %s since %s, heartbeat %s",
		stats.Lease.Holder,
		stats.Lease.AcquiredAt.UTC().Format(time.RFC3339),
		humanizeAge(now.Sub(stats.Lease.HeartbeatAt)))
}
