package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport/mcp"
)

const dateLayout = "2006-01-02"

// Matches the width the service abbreviates SHAs to in gap text.
const blamedSHAChars = 12

const noEvidence = "no evidence found — nothing links to this document yet, or run `lore sync` if the trail should be there"

func printfln(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// A negative age reads as "just now".
func humanizeAge(d time.Duration) string {
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

func indent(text, pad string) string {
	lines := strings.Split(strings.TrimRight(text, " \t\n"), "\n")
	for i, line := range lines {
		lines[i] = pad + strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

func shortSHAs(shas []string) string {
	short := make([]string, len(shas))
	for i, sha := range shas {
		short[i] = sha[:min(len(sha), blamedSHAChars)]
	}
	return strings.Join(short, ", ")
}

func fileWithSpan(code *entities.CodeAnchor) string {
	if code.LineStart == 0 {
		return code.File
	}

	return fmt.Sprintf("%s:%d-%d", code.File, code.LineStart, code.LineEnd)
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

type evidenceOutput struct {
	raw     bool
	explain bool
}

func (o *evidenceOutput) flags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.BoolVar(&o.raw, "raw", false, "emit the evidence bundle as JSON instead of the timeline")
	f.BoolVar(&o.explain, "explain", false,
		"explain the evidence as prose instead of the timeline; needs the llm: block in lore.yaml")
}

func (o evidenceOutput) emit(cmd *cobra.Command, synthesis services.SynthesisService, bundle *entities.EvidenceBundle) error {
	switch {
	case o.raw:
		return writeJSON(cmd.OutOrStdout(), bundle)
	case o.explain:
		return emitProse(cmd, synthesis, bundle)
	}
	renderBundle(cmd.OutOrStdout(), bundle)

	return nil
}

func emitProse(cmd *cobra.Command, synthesis services.SynthesisService, bundle *entities.EvidenceBundle) error {
	answer, err := synthesis.Synthesize(cmd.Context(), bundle.Question, bundle)
	if err != nil {
		return err
	}
	printfln(cmd.OutOrStdout(), "%s", answer)

	return nil
}

func renderBundle(w io.Writer, bundle *entities.EvidenceBundle) {
	printfln(w, "%s", bundle.Question)
	if code := bundle.Anchor.Code; code != nil {
		printfln(w, "anchor: %s %s", code.Repo, fileWithSpan(code))
		if len(code.BlamedSHAs) > 0 {
			printfln(w, "        blamed %s", shortSHAs(code.BlamedSHAs))
		}
	}
	if doc := bundle.Anchor.Doc; doc != nil {
		printfln(w, "anchor: %s", doc.Title)
		printfln(w, "        %s", doc.URL)
	}
	if window := bundle.Anchor.Window; window != nil {
		printfln(w, "window: %s .. %s (%s)",
			window.From.UTC().Format(dateLayout), window.To.UTC().Format(dateLayout), window.Derivation)
	}
	printfln(w, "")

	if len(bundle.Nodes) == 0 {
		printfln(w, "%s", noEvidence)
	} else {
		renderNodes(w, bundle.Nodes)
	}

	renderChains(w, bundle.Chains)
	renderGaps(w, bundle.Gaps)
}

func renderNodes(w io.Writer, nodes []entities.EvidenceNode) {
	printfln(w, "%s", plural(len(nodes), "document", "documents"))
	for _, node := range nodes {
		printfln(w, "")
		printfln(w, "%s %s", entryLead(node), node.Doc.Title)
		printfln(w, "   %s", metaLine(node))
		printfln(w, "   %s", node.Doc.URL)
		if excerpt := strings.TrimSpace(node.Excerpt); excerpt != "" {
			printfln(w, "%s", indent(excerpt, "      "))
		}
	}
}

func entryLead(node entities.EvidenceNode) string {
	if node.Doc.CreatedAt.IsZero() {
		return "undated"
	}
	return node.Doc.CreatedAt.UTC().Format(dateLayout)
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
