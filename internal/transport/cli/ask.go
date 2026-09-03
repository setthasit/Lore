package cli

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/services"
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
		Long: "Retrieves the documents that explain a decision, then answers from them in\n" +
			"prose that cites the documents it used and lists their source URLs. The\n" +
			"answer needs the llm: block in lore.yaml; --raw emits the evidence bundle\n" +
			"as JSON for scripting instead, and needs no LLM.",
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

				return emitProse(cmd, rt.Synthesis, bundle)
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
