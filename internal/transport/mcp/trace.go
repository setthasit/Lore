package mcp

import (
	"context"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/setthasit/Lore/internal/services"
)

const traceName = "trace"

const traceDescription = `Trace one document's provenance neighborhood: what it came from, what came out of it, and its own text up to 8,000 characters.

Use this for depth on one document; use find_decision for breadth across a decision, impact_of for the consequences of a decision, history_of for how one file evolved.

Returns an evidence bundle, not an answer: the anchor document with its body as the excerpt, its linked neighbors, the chains connecting them, and a gap for every trail that dead-ends. Nothing is synthesized here — you write the account from these citations, and every claim you make should point at one of their URLs.

A body over 8,000 characters is cut to the first 8,000 and ends with a marker that gives the full length. Pass focus with a question to get up to 3 passages of the anchor that best match it instead, in document order. A … line marks skipped text between passages that are not adjacent. A focus is at most 1,000 characters, and a longer one is rejected. A focus that matches no passage returns the body as if no focus was given, plus a gap that says so. Neighbors and chains are the same with or without focus.`

type traceInput struct {
	Ref       string `json:"ref" jsonschema:"the document to trace: a commit SHA, a pull request or issue number, a ticket key, a document URL or a document id"`
	Direction string `json:"direction,omitempty" jsonschema:"which links to follow: out for the documents this one references, in for the documents that reference it, both for either"`
	Depth     int    `json:"depth,omitempty" jsonschema:"how many link hops to follow from the document; omit it for the server default"`
	Focus     string `json:"focus,omitempty" jsonschema:"a question; when set, the anchor excerpt holds only the passages of the document that answer it"`
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
		Focus:     in.Focus,
	})
	if err != nil {
		return nil, evidenceBundle{}, toolError(t.log, traceName, err)
	}

	return nil, newEvidenceBundle(bundle), nil
}
