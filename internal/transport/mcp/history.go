package mcp

import (
	"context"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lore/internal/services"
)

const historyName = "history_of"

const historyDescription = `Follow how one file evolved: walk the commits that touched it in a registered local clone, then pick up the pull requests, reviews, tickets and design pages linked to each of them.

Use this for the evolution of a whole file; use why for breadth on a span of lines you can name, trace for depth on one document, impact_of for the consequences of one decision.

Returns an evidence bundle, not an answer: one page of the file's history in chronological order, each document with an excerpt and the URL it came from, and a gap for every trail that dead-ends. Nothing is synthesized here — you write the account from these citations, and every claim you make should point at one of their URLs.

The bundle is one page, newest commits first: limit sizes it and the server caps it, and before pages backwards. To fetch the next, older page, pass the last entry of anchor.code.blamed_shas as before; an empty blamed_shas means the history is exhausted. Do not page from a node id: nodes are sorted oldest-first, may lead with a linked pull request or issue, and their ids are document ids rather than commit SHAs.

This tool needs the file's repository registered as a local clone; a workspace that registers none cannot anchor on code at all, and find_decision answers the same question from the index instead.`

type historyInput struct {
	Path   string `json:"path" jsonschema:"path of the file whose history to walk, relative to the repository root"`
	Repo   string `json:"repo,omitempty" jsonschema:"the repository the file belongs to, named as it is registered: a remote such as github:owner/repo or the clone's path; omit it when only one repository is registered"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many commits the page holds; omit it for the server default, and expect the server to cap whatever you ask for"`
	Before string `json:"before,omitempty" jsonschema:"commit SHA, abbreviated or full, to page backwards from: the page holds the commits older than it. Pass the last entry of anchor.code.blamed_shas from the previous bundle, never a node id; an empty blamed_shas means the history is exhausted"`
}

type historyTool struct {
	history services.HistoryService
	log     *slog.Logger
}

func registerHistoryOf(server *sdk.Server, history services.HistoryService, log *slog.Logger) {
	tool := historyTool{history: history, log: log}
	sdk.AddTool(server, &sdk.Tool{
		Name:        historyName,
		Description: historyDescription,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, tool.handle)
}

func (t historyTool) handle(ctx context.Context, _ *sdk.CallToolRequest, in historyInput) (*sdk.CallToolResult, evidenceBundle, error) {
	bundle, err := t.history.HistoryOf(ctx, services.HistoryRequest{
		Repo:   in.Repo,
		File:   in.Path,
		Limit:  in.Limit,
		Before: in.Before,
	})
	if err != nil {
		return nil, evidenceBundle{}, toolError(t.log, historyName, err)
	}

	return nil, newEvidenceBundle(bundle), nil
}
