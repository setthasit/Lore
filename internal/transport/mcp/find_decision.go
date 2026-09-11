package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/services"
)

const findDecisionName = "find_decision"

const findDecisionDescription = `Find the recorded evidence behind a decision: the commits, pull requests, reviews, tickets and design pages that explain why it was made.

Use this for breadth across a decision; use trace for depth on one document, impact_of for the consequences of a decision, history_of for how one file evolved.

Returns an evidence bundle, not an answer: cited documents ordered by relevance, each with an excerpt and the URL it came from. Nothing is synthesized here — you write the explanation from these citations, and every claim you make should point at one of their URLs. An empty bundle is a real answer: the index holds no evidence for that question, so widen the filters or rephrase it.`

const dateLayout = "2006-01-02"

type findDecisionInput struct {
	Question string `json:"question" jsonschema:"the question to answer, in natural language"`
	Around   string `json:"around,omitempty" jsonschema:"event or date the question is anchored to, such as 'incident X' or 2025-03-12"`
	Source   string `json:"source,omitempty" jsonschema:"keep only evidence from one source instance, named by the id it has in the workspace configuration"`
	Repo     string `json:"repo,omitempty" jsonschema:"keep only evidence from one repository, named as <forge>:owner/repo"`
	DocType  string `json:"doc_type,omitempty" jsonschema:"keep only evidence of one document type, such as commit, pr, issue, ticket or page"`
	Since    string `json:"since,omitempty" jsonschema:"keep only evidence created at or after this date (YYYY-MM-DD, from its first instant) or RFC 3339 timestamp"`
	Until    string `json:"until,omitempty" jsonschema:"keep only evidence created at or before this date (YYYY-MM-DD, through its last instant) or RFC 3339 timestamp"`
}

type findDecisionTool struct {
	query services.QueryService
	log   *slog.Logger
}

func registerFindDecision(server *sdk.Server, query services.QueryService, log *slog.Logger) {
	tool := findDecisionTool{query: query, log: log}
	sdk.AddTool(server, &sdk.Tool{
		Name:        findDecisionName,
		Description: findDecisionDescription,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, tool.handle)
}

func (t findDecisionTool) handle(ctx context.Context, _ *sdk.CallToolRequest, in findDecisionInput) (*sdk.CallToolResult, evidenceBundle, error) {
	req, err := in.serviceRequest()
	if err != nil {
		return nil, evidenceBundle{}, toolError(t.log, findDecisionName, err)
	}

	bundle, err := t.query.FindDecision(ctx, req)
	if err != nil {
		return nil, evidenceBundle{}, toolError(t.log, findDecisionName, err)
	}

	return nil, newEvidenceBundle(bundle), nil
}

func (in findDecisionInput) serviceRequest() (services.FindDecisionRequest, error) {
	since, err := parseWindowBound("since", in.Since, dayStart)
	if err != nil {
		return services.FindDecisionRequest{}, err
	}
	until, err := parseWindowBound("until", in.Until, dayEnd)
	if err != nil {
		return services.FindDecisionRequest{}, err
	}

	return services.FindDecisionRequest{
		Question: in.Question,
		Around:   in.Around,
		Source:   in.Source,
		Repo:     in.Repo,
		DocType:  in.DocType,
		Since:    since,
		Until:    until,
	}, nil
}

type dayEdge int

const (
	dayStart dayEdge = iota
	dayEnd
)

// The store's bounds are inclusive and its timestamps have second precision.
const lastInstantOfDay = 24*time.Hour - time.Second

func parseWindowBound(field, raw string, edge dayEdge) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if day, err := time.Parse(dateLayout, raw); err == nil {
		if edge == dayEnd {
			return day.Add(lastInstantOfDay), nil
		}
		return day, nil
	}
	if bound, err := time.Parse(time.RFC3339, raw); err == nil {
		return bound, nil
	}

	return time.Time{}, internalerror.NewBadRequestError(
		fmt.Sprintf("%s: %q is neither a date (YYYY-MM-DD) nor an RFC 3339 timestamp", field, raw), nil)
}
