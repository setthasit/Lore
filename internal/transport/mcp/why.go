package mcp

import (
	"context"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lore/internal/services"
)

const whyName = "why"

const whyDescription = `Explain why a span of code is the way it is: blame those lines in a registered local clone, then follow the commits, pull requests, reviews, tickets and design pages behind them.

Use this when the question points at code you can name by file and line; use find_decision for breadth when there is no line to point at, trace for depth on one document, impact_of for the consequences of a decision.

Returns an evidence bundle, not an answer: the documents behind those lines with an excerpt and the URL each came from, and a gap for every trail that dead-ends. Nothing is synthesized here — you write the explanation from these citations, and every claim you make should point at one of their URLs. This tool needs the file's repository registered as a local clone; a workspace that registers none cannot anchor on code at all, and find_decision answers the same question from the index instead.`

type whyInput struct {
	Repo      string `json:"repo,omitempty" jsonschema:"the repository the file belongs to, named as it is registered: a remote such as github:owner/repo or the clone's path; omit it when only one repository is registered"`
	File      string `json:"file" jsonschema:"path of the file to blame, relative to the repository root"`
	LineStart int    `json:"line_start" jsonschema:"first line of the span to blame, counting from 1"`
	LineEnd   int    `json:"line_end,omitempty" jsonschema:"last line of the span to blame; omit it to blame line_start alone"`
	Question  string `json:"question,omitempty" jsonschema:"narrows the explanation to one concern, in natural language"`
}

type whyTool struct {
	why services.WhyService
	log *slog.Logger
}

func registerWhy(server *sdk.Server, why services.WhyService, log *slog.Logger) {
	tool := whyTool{why: why, log: log}
	sdk.AddTool(server, &sdk.Tool{
		Name:        whyName,
		Description: whyDescription,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, tool.handle)
}

func (t whyTool) handle(ctx context.Context, _ *sdk.CallToolRequest, in whyInput) (*sdk.CallToolResult, evidenceBundle, error) {
	bundle, err := t.why.Why(ctx, services.WhyRequest{
		Repo:      in.Repo,
		File:      in.File,
		LineStart: in.LineStart,
		LineEnd:   in.LineEnd,
		Question:  in.Question,
	})
	if err != nil {
		return nil, evidenceBundle{}, toolError(t.log, whyName, err)
	}

	return nil, newEvidenceBundle(bundle), nil
}
