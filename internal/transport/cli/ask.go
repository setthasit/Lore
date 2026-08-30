package cli

import (
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/services"
	"lore/internal/transport/mcp"
)

type askFlags struct {
	around  string
	source  string
	repo    string
	docType string
	since   string
	until   string
	raw     bool
}

func newAskCommand(resolve Resolver, configPath *string) *cobra.Command {
	var flags askFlags

	cmd := &cobra.Command{
		Use:   "ask <question>",
		Short: "Answer a question from the indexed decision trail, with citations",
		Long: "Retrieves the documents that explain a decision and prints them with their\n" +
			"source URLs. Every claim you can make from the output is traceable to one of\n" +
			"the cited documents; --raw emits the evidence bundle as JSON for scripting.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := flags.request(args[0])
			if err != nil {
				return err
			}
			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				bundle, err := rt.Query.FindDecision(cmd.Context(), req)
				if err != nil {
					return err
				}
				if flags.raw {
					return writeJSON(cmd.OutOrStdout(), bundle)
				}
				renderBundle(cmd.OutOrStdout(), bundle)
				return nil
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&flags.around, "around", "", "event text or date the question is anchored to")
	f.StringVar(&flags.source, "source", "", "only cite documents from this source (github, notion, jira)")
	f.StringVar(&flags.repo, "repo", "", "only cite documents from this repository (owner/name)")
	f.StringVar(&flags.docType, "doc-type", "", "only cite documents of this type (commit, pr, issue, page, ticket, …)")
	f.StringVar(&flags.since, "since", "", "only cite documents created on or after this date (YYYY-MM-DD, from 00:00:00Z, or an RFC 3339 timestamp)")
	f.StringVar(&flags.until, "until", "", "only cite documents created on or before this date (YYYY-MM-DD, inclusive — covers the whole day — or an RFC 3339 timestamp)")
	f.BoolVar(&flags.raw, "raw", false, "emit the evidence bundle as JSON instead of prose")
	return cmd
}

func (f askFlags) request(question string) (services.FindDecisionRequest, error) {
	since, err := parseTimeFlag("since", f.since, startOfDay)
	if err != nil {
		return services.FindDecisionRequest{}, err
	}
	until, err := parseTimeFlag("until", f.until, endOfDay)
	if err != nil {
		return services.FindDecisionRequest{}, err
	}

	return services.FindDecisionRequest{
		Question: question,
		Around:   f.around,
		Source:   f.source,
		Repo:     f.repo,
		DocType:  f.docType,
		Since:    since,
		Until:    until,
	}, nil
}

const dateLayout = "2006-01-02"

type dayBound func(time.Time) time.Time

func startOfDay(day time.Time) time.Time { return day }

// Timestamps are stored at second precision, so a bare --until ends at 23:59:59.
func endOfDay(day time.Time) time.Time {
	return day.Add(24*time.Hour - time.Second)
}

func parseTimeFlag(flag, value string, bound dayBound) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if day, err := time.Parse(dateLayout, value); err == nil {
		return bound(day.UTC()), nil
	}
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at.UTC(), nil
	}
	return time.Time{}, internalerror.NewBadRequestError(
		"--"+flag+" "+value+" is not a date: use YYYY-MM-DD or an RFC 3339 timestamp", nil)
}

func writeJSON(w io.Writer, bundle *entities.EvidenceBundle) error {
	encoded, err := mcp.EncodeBundle(bundle)
	if err != nil {
		return internalerror.NewInternalError("cannot encode the evidence bundle", err)
	}
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		return internalerror.NewInternalError("cannot write the evidence bundle", err)
	}
	return nil
}

func renderBundle(w io.Writer, bundle *entities.EvidenceBundle) {
	printfln(w, "%s", bundle.Question)
	if window := bundle.Anchor.Window; window != nil {
		printfln(w, "window: %s .. %s (%s)",
			window.From.UTC().Format(dateLayout), window.To.UTC().Format(dateLayout), window.Derivation)
	}
	printfln(w, "")

	if len(bundle.Nodes) == 0 {
		printfln(w, "no evidence found — widen the filters, or run `lore sync` if the trail should be there")
		return
	}

	printfln(w, "%s", plural(len(bundle.Nodes), "document", "documents"))
	for i, node := range bundle.Nodes {
		printfln(w, "")
		printfln(w, "%d. %s", i+1, node.Doc.Title)
		printfln(w, "   %s", metaLine(node))
		printfln(w, "   %s", node.Doc.URL)
		if excerpt := strings.TrimSpace(node.Excerpt); excerpt != "" {
			printfln(w, "%s", indent(excerpt, "      "))
		}
	}

	renderChains(w, bundle.Chains)
	renderGaps(w, bundle.Gaps)
}

func metaLine(node entities.EvidenceNode) string {
	parts := []string{node.Doc.Source + " " + string(node.Doc.Type)}
	if node.Doc.Author != "" {
		parts = append(parts, node.Doc.Author)
	}
	if !node.Doc.CreatedAt.IsZero() {
		parts = append(parts, node.Doc.CreatedAt.UTC().Format(dateLayout))
	}
	if node.Role != "" && node.Role != entities.RoleSeed {
		parts = append(parts, node.Role)
	}
	return strings.Join(parts, " · ")
}

func renderChains(w io.Writer, chains [][]entities.DocID) {
	if len(chains) == 0 {
		return
	}
	printfln(w, "")
	printfln(w, "chains:")
	for _, chain := range chains {
		ids := make([]string, len(chain))
		for i, id := range chain {
			ids[i] = string(id)
		}
		printfln(w, "  %s", strings.Join(ids, " → "))
	}
}

func renderGaps(w io.Writer, gaps []string) {
	if len(gaps) == 0 {
		return
	}
	printfln(w, "")
	printfln(w, "gaps:")
	for _, gap := range gaps {
		printfln(w, "  %s", gap)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
