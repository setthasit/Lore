package services

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/setthasit/Lore/internal/connectors/llm"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
)

type SynthesisService interface {
	// The prose cites bundle.Nodes as [n], 1-based in slice order, and ends with
	// the source list for those numbers.
	Synthesize(ctx context.Context, question string, bundle *entities.EvidenceBundle) (string, error)
}

type synthesisService struct {
	model llm.LLM
}

var _ SynthesisService = (*synthesisService)(nil)

// A nil model is a workspace with no llm: block: every non-synthesizing verb stays usable.
func NewSynthesisService(model llm.LLM) SynthesisService {
	return &synthesisService{model: model}
}

const synthesisSystem = `You explain the history of a software project from evidence supplied with the question.

- Answer ONLY from the numbered evidence below. Never add a fact from your own knowledge.
- Cite the evidence number for every claim, inline, as [1] or [2][5]. A sentence without a citation is not allowed.
- Write inline citations only. Never write a source list, a bibliography, a URL or a document title in place of a number.
- Report what the evidence does not settle, using the listed gaps. Never fill a gap with a guess.
- When the evidence is a chronological timeline, keep that order in your answer.
- Answer in markdown prose: short paragraphs, no headings.`

const unconfiguredSynthesis = "synthesis needs an LLM, and this workspace has no llm: block in lore.yaml — " +
	"add one naming the provider, the model and the api_key_env that holds its key"

var citation = regexp.MustCompile(`\[\s*(\d+)\s*\]`)

func (s *synthesisService) Synthesize(ctx context.Context, question string, bundle *entities.EvidenceBundle) (string, error) {
	if s.model == nil {
		return "", internalerror.NewPreconditionError(unconfiguredSynthesis, nil)
	}
	if bundle == nil {
		return "", internalerror.NewBadRequestError("cannot synthesize an answer without an evidence bundle", nil)
	}

	answer, err := s.model.Complete(ctx, synthesisSystem, synthesisPrompt(question, bundle))
	if err != nil {
		return "", internalerror.NewInternalError("the configured LLM did not answer the question", err)
	}

	answer = strings.TrimSpace(answer)
	if err := checkCitations(answer, len(bundle.Nodes)); err != nil {
		return "", err
	}
	return answer + sourceList(bundle.Nodes), nil
}

func checkCitations(answer string, nodes int) error {
	cited := citation.FindAllStringSubmatchIndex(answer, -1)
	for _, match := range cited {
		raw := answer[match[2]:match[3]]
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > nodes {
			return internalerror.NewInternalError(fmt.Sprintf(
				"the answer cites [%s], but the evidence is numbered 1 to %d: that citation resolves to no document",
				raw, nodes), nil)
		}
		if end := match[1]; end < len(answer) && answer[end] == '(' {
			return internalerror.NewInternalError(fmt.Sprintf(
				"the answer wrote citation [%d] as a markdown link: a citation is the bare number, and the source list carries the URLs",
				n), nil)
		}
	}
	if nodes > 0 && len(cited) == 0 {
		return internalerror.NewInternalError(fmt.Sprintf(
			"the answer cites none of the %d evidence documents, so no claim in it is grounded", nodes), nil)
	}
	return nil
}

func sourceList(nodes []entities.EvidenceNode) string {
	if len(nodes) == 0 {
		return ""
	}

	var out strings.Builder
	out.WriteString("\n\n**Sources**\n\n")
	for i, node := range nodes {
		fmt.Fprintf(&out, "%d. %s — %s\n", i+1, node.Doc.Title, node.Doc.URL)
	}
	return out.String()
}

func synthesisPrompt(question string, bundle *entities.EvidenceBundle) string {
	var out strings.Builder
	out.WriteString("Question: " + strings.TrimSpace(question) + "\n")
	out.WriteString("Anchor: " + anchorSummary(bundle.Anchor) + "\n")

	out.WriteString("\nEvidence — cite these numbers as [n]:\n")
	if len(bundle.Nodes) == 0 {
		out.WriteString("\n(no documents were found; there is nothing to cite)\n")
	}
	numbers := make(map[entities.DocID]int, len(bundle.Nodes))
	for i, node := range bundle.Nodes {
		numbers[node.Doc.ID] = i + 1
		writeNode(&out, i+1, node)
	}

	out.WriteString("\nProvenance chains:\n")
	writeLines(&out, chainLines(bundle.Chains, numbers))

	out.WriteString("\nGaps in the evidence:\n")
	writeLines(&out, bundle.Gaps)

	return out.String()
}

func writeNode(out *strings.Builder, number int, node entities.EvidenceNode) {
	fmt.Fprintf(out, "\n[%d] %s\n", number, node.Doc.Title)
	fmt.Fprintf(out, "    source: %s | type: %s | role: %s\n", node.Doc.Source, node.Doc.Type, node.Role)
	fmt.Fprintf(out, "    author: %s | created: %s\n", orUnknown(node.Doc.Author), promptTime(node.Doc.CreatedAt))
	fmt.Fprintf(out, "    url: %s\n", node.Doc.URL)
	fmt.Fprintf(out, "    excerpt:\n%s\n", node.Excerpt)
}

func writeLines(out *strings.Builder, lines []string) {
	if len(lines) == 0 {
		out.WriteString("- (none recorded)\n")
		return
	}
	for _, line := range lines {
		out.WriteString("- " + line + "\n")
	}
}

func chainLines(chains [][]entities.DocID, numbers map[entities.DocID]int) []string {
	lines := make([]string, 0, len(chains))
	for _, chain := range chains {
		hops := make([]string, 0, len(chain))
		for _, id := range chain {
			if number, cited := numbers[id]; cited {
				hops = append(hops, fmt.Sprintf("[%d]", number))
				continue
			}
			hops = append(hops, string(id))
		}
		if len(hops) > 0 {
			lines = append(lines, strings.Join(hops, " -> "))
		}
	}
	return lines
}

func anchorSummary(anchor entities.Anchor) string {
	var parts []string
	if anchor.Query != "" {
		parts = append(parts, "question "+strconv.Quote(anchor.Query))
	}
	if code := anchor.Code; code != nil {
		parts = append(parts, codeSummary(*code))
	}
	if doc := anchor.Doc; doc != nil {
		parts = append(parts, fmt.Sprintf("document %q (%s) created %s", doc.Title, doc.URL, promptTime(doc.CreatedAt)))
	}
	if window := anchor.Window; window != nil {
		parts = append(parts, fmt.Sprintf("time window %s .. %s (%s)",
			promptTime(window.From), promptTime(window.To), orUnknown(window.Derivation)))
	}
	if len(parts) == 0 {
		return "unanchored"
	}
	return strings.Join(parts, "; ")
}

func codeSummary(code entities.CodeAnchor) string {
	span := code.File
	if code.LineStart > 0 {
		span = fmt.Sprintf("%s:%d-%d", code.File, code.LineStart, code.LineEnd)
	}
	return fmt.Sprintf("code %s %s blamed on %s", code.Repo, span, orUnknown(strings.Join(code.BlamedSHAs, ", ")))
}

func promptTime(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	return at.UTC().Format(time.RFC3339)
}

func orUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
