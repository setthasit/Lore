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

// renderStatus prints the index's state. now is passed in rather than read here
// so the ages a run prints all refer to the same instant.
func renderStatus(w io.Writer, stats entities.IndexStats, now time.Time) {
	printfln(w, "documents: %d", stats.Documents)
	printfln(w, "chunks:    %d", stats.Chunks)

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
	// A held lease is reported with the age of its heartbeat and nothing more:
	// whether the holder is still alive is a judgement the store's lease TTL
	// makes when the next round tries to take over, not one a status line should
	// duplicate.
	if stats.Lease == nil {
		printfln(w, "sync lock: free")
		return
	}
	printfln(w, "sync lock: held by %s since %s, heartbeat %s",
		stats.Lease.Holder,
		stats.Lease.AcquiredAt.UTC().Format(time.RFC3339),
		humanizeAge(now.Sub(stats.Lease.HeartbeatAt)))
}
