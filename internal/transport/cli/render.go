package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/transport/mcp"
)

const dateLayout = "2006-01-02"

type bundleView int

const (
	viewRelevance bundleView = iota
	viewTimeline
)

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

func emitBundle(w io.Writer, bundle *entities.EvidenceBundle, raw bool, view bundleView) error {
	if raw {
		return writeJSON(w, bundle)
	}
	renderBundle(w, bundle, view)

	return nil
}

func renderBundle(w io.Writer, bundle *entities.EvidenceBundle, view bundleView) {
	printfln(w, "%s", bundle.Question)
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
		printfln(w, "%s", view.emptyMessage())
	} else {
		renderNodes(w, bundle.Nodes, view)
	}

	renderChains(w, bundle.Chains)
	renderGaps(w, bundle.Gaps)
}

func renderNodes(w io.Writer, nodes []entities.EvidenceNode, view bundleView) {
	printfln(w, "%s", plural(len(nodes), "document", "documents"))
	for i, node := range nodes {
		printfln(w, "")
		printfln(w, "%s %s", view.entryLead(i, node), node.Doc.Title)
		printfln(w, "   %s", metaLine(node))
		printfln(w, "   %s", node.Doc.URL)
		if excerpt := strings.TrimSpace(node.Excerpt); excerpt != "" {
			printfln(w, "%s", indent(excerpt, "      "))
		}
	}
}

func (v bundleView) entryLead(index int, node entities.EvidenceNode) string {
	if v == viewRelevance {
		return strconv.Itoa(index+1) + "."
	}
	if node.Doc.CreatedAt.IsZero() {
		return "undated"
	}
	return node.Doc.CreatedAt.UTC().Format(dateLayout)
}

func (v bundleView) emptyMessage() string {
	if v == viewRelevance {
		return "no evidence found — widen the filters, or run `lore sync` if the trail should be there"
	}
	return "no evidence found — nothing links to this document yet, or run `lore sync` if the trail should be there"
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
