package mcp

import (
	"context"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/setthasit/Lore/internal/services"
)

const impactName = "impact_of"

const impactDescription = `Find what followed from a decision: the changes, follow-ups, reverts and discussions that came after it.

Use this for the consequences of a decision; use find_decision for breadth across a decision, trace for depth on one document, history_of for how one file evolved.

Returns an evidence bundle, not an answer: the documents that came after the anchor in chronological order, each with an excerpt and the URL it came from, and a gap for every trail that dead-ends. Nothing is synthesized here — you write the account from these citations. An empty bundle is a real answer: the index records nothing that followed that decision.`

type impactInput struct {
	RefOrQuery string `json:"ref_or_query" jsonschema:"the decision to start from: a commit SHA, a pull request or issue number, a ticket key, a document URL, a document id, or free text naming the decision"`
	Question   string `json:"question,omitempty" jsonschema:"narrows the consequences to one concern, in natural language"`
}

type impactTool struct {
	impact services.ImpactService
	log    *slog.Logger
}

func registerImpactOf(server *sdk.Server, impact services.ImpactService, log *slog.Logger) {
	tool := impactTool{impact: impact, log: log}
	sdk.AddTool(server, &sdk.Tool{
		Name:        impactName,
		Description: impactDescription,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, tool.handle)
}

func (t impactTool) handle(ctx context.Context, _ *sdk.CallToolRequest, in impactInput) (*sdk.CallToolResult, evidenceBundle, error) {
	bundle, err := t.impact.ImpactOf(ctx, services.ImpactRequest{
		Ref:      in.RefOrQuery,
		Question: in.Question,
	})
	if err != nil {
		return nil, evidenceBundle{}, toolError(t.log, impactName, err)
	}

	return nil, newEvidenceBundle(bundle), nil
}
