package mcp

import (
	"context"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/setthasit/Lore/internal/services"
)

const traceName = "trace"

const traceDescription = `Trace one document's provenance neighborhood: what it came from, what came out of it, and its own full text.

Use this for depth on one document; use find_decision for breadth across a decision, impact_of for the consequences of a decision, history_of for how one file evolved.

Returns an evidence bundle, not an answer: the anchor document with its whole body rather than an excerpt, its linked neighbors, the chains connecting them, and a gap for every trail that dead-ends. Nothing is synthesized here — you write the account from these citations, and every claim you make should point at one of their URLs.`

type traceInput struct {
	Ref       string `json:"ref" jsonschema:"the document to trace: a commit SHA, a pull request or issue number, a ticket key, a document URL or a document id"`
	Direction string `json:"direction,omitempty" jsonschema:"which links to follow: out for the documents this one references, in for the documents that reference it, both for either"`
	Depth     int    `json:"depth,omitempty" jsonschema:"how many link hops to follow from the document; omit it for the server default"`
}

type traceTool struct {
	trace services.TraceService
	log   *slog.Logger
}

func registerTrace(server *sdk.Server, trace services.TraceService, log *slog.Logger) {
	tool := traceTool{trace: trace, log: log}
	sdk.AddTool(server, &sdk.Tool{
		Name:        traceName,
		Description: traceDescription,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, tool.handle)
}

func (t traceTool) handle(ctx context.Context, _ *sdk.CallToolRequest, in traceInput) (*sdk.CallToolResult, evidenceBundle, error) {
	bundle, err := t.trace.Trace(ctx, services.TraceRequest{
		Ref:       in.Ref,
		Direction: in.Direction,
		Depth:     in.Depth,
	})
	if err != nil {
		return nil, evidenceBundle{}, toolError(t.log, traceName, err)
	}

	return nil, newEvidenceBundle(bundle), nil
}
