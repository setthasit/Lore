package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/services"
)

func bundleFixture() *entities.EvidenceBundle {
	at := time.Date(2025, time.March, 12, 9, 30, 0, 0, time.UTC)
	return &entities.EvidenceBundle{
		Question: "why did we pick sqlite?",
		Anchor:   entities.Anchor{Kind: entities.AnchorQuery, Query: "why did we pick sqlite?"},
		Nodes: []entities.EvidenceNode{{
			Doc: entities.DocumentMeta{
				ID:        entities.NewDocID("github", entities.DocTypePR, "12"),
				Source:    "github",
				Type:      entities.DocTypePR,
				Title:     "Index on SQLite, not Postgres",
				Author:    "dev@example.test",
				URL:       "https://github.com/acme/lore/pull/12",
				CreatedAt: at,
			},
			Excerpt: "sqlite ships everywhere and needs no server\nso the workspace file is the whole deployment",
			Role:    entities.RoleSeed,
			Score:   0.91,
		}, {
			Doc: entities.DocumentMeta{
				ID:        entities.NewDocID("notion", entities.DocTypePage, "design/storage"),
				Source:    "notion",
				Type:      entities.DocTypePage,
				Title:     "Storage design",
				Author:    "arch@example.test",
				URL:       "https://notion.so/design/storage",
				CreatedAt: at.Add(-48 * time.Hour),
			},
			Excerpt: "postgres with pgvector was the alternative",
			Role:    entities.RoleFollowUp,
			Score:   0.44,
		}},
		Gaps: []string{"trail ends at PROJ-4521; no linked follow-up"},
	}
}

func TestAskAnswersInSynthesizedProse(t *testing.T) {
	rt, query := mockQuery(t)
	synthesis := mockSynthesis(t, rt)
	bundle := bundleFixture()
	query.EXPECT().
		FindDecision(gomock.Any(), services.FindDecisionRequest{Question: "why sqlite?"}).
		Return(bundle, nil)
	synthesis.EXPECT().Synthesize(gomock.Any(), bundle.Question, bundle).Return(proseAnswer, nil)

	res := run(t, rt, "ask", "why sqlite?")
	wantProse(t, res)
	if !res.released {
		t.Error("the workspace was not released")
	}
	for _, unwanted := range []string{"2 documents", "gaps:", string(entities.RoleFollowUp)} {
		if strings.Contains(res.stdout, unwanted) {
			t.Errorf("stdout carries the pretty-printed bundle (%q):\n%s", unwanted, res.stdout)
		}
	}
}

func TestAskWithoutAnLLMSurfacesThePrecondition(t *testing.T) {
	rt, query := mockQuery(t)
	unconfigured := services.NewSynthesisService(nil)
	rt.Synthesis = unconfigured
	query.EXPECT().FindDecision(gomock.Any(), gomock.Any()).Return(bundleFixture(), nil)

	_, refused := unconfigured.Synthesize(context.Background(), "why sqlite?", bundleFixture())
	if !internalerror.IsPrecondition(refused) {
		t.Fatalf("Synthesize() with no LLM = %v, want a precondition error", refused)
	}

	res := run(t, rt, "ask", "why sqlite?")
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitPrecondition, res.stderr)
	}
	if res.stderr != "lore: "+refused.Error()+"\n" {
		t.Errorf("stderr = %q, want the precondition verbatim (%q)", res.stderr, refused)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want nothing printed", res.stdout)
	}
	if !res.released {
		t.Error("the workspace was not released after a failure")
	}
}

func TestAskEmptyBundleIsAnAnswerNotAnError(t *testing.T) {
	rt, query := mockQuery(t)
	synthesis := mockSynthesis(t, rt)
	empty := &entities.EvidenceBundle{Question: "why sqlite?"}
	query.EXPECT().FindDecision(gomock.Any(), gomock.Any()).Return(empty, nil)
	synthesis.EXPECT().Synthesize(gomock.Any(), empty.Question, empty).Return(proseAnswer, nil)

	res := run(t, rt, "ask", "why sqlite?")
	wantProse(t, res)
}

func TestAskRawEmitsTheCanonicalBundleJSON(t *testing.T) {
	rt, query := mockQuery(t)
	mockSynthesis(t, rt)
	bundle := bundleFixture()
	query.EXPECT().FindDecision(gomock.Any(), gomock.Any()).Return(bundle, nil)

	res := run(t, rt, "ask", "why sqlite?", "--raw")
	wantBundleJSON(t, res, bundle)

	var decoded map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, res.stdout)
	}
	if decoded["question"] != bundle.Question {
		t.Errorf("question = %v, want %q", decoded["question"], bundle.Question)
	}
	nodes, ok := decoded["nodes"].([]any)
	if !ok || len(nodes) != len(bundle.Nodes) {
		t.Fatalf("nodes = %v, want %d entries", decoded["nodes"], len(bundle.Nodes))
	}
	first, _ := nodes[0].(map[string]any)
	doc, _ := first["doc"].(map[string]any)
	if doc["url"] != bundle.Nodes[0].Doc.URL {
		t.Errorf("first url = %v, want %q", doc["url"], bundle.Nodes[0].Doc.URL)
	}
}

func TestAskPassesFiltersThrough(t *testing.T) {
	rt, query := mockQuery(t)
	mockSynthesis(t, rt).EXPECT().Synthesize(gomock.Any(), gomock.Any(), gomock.Any()).Return(proseAnswer, nil)
	query.EXPECT().FindDecision(gomock.Any(), services.FindDecisionRequest{
		Question: "why sqlite?",
		Around:   "incident X",
		Source:   "github",
		Repo:     "acme/lore",
		DocType:  "pr",
		Since:    time.Date(2025, time.March, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2025, time.March, 31, 23, 59, 59, 0, time.UTC),
	}).Return(&entities.EvidenceBundle{Question: "why sqlite?"}, nil)

	res := run(t, rt, "ask", "why sqlite?",
		"--around", "incident X",
		"--source", "github",
		"--repo", "acme/lore",
		"--doc-type", "pr",
		"--since", "2025-03-01",
		"--until", "2025-03-31T23:59:59Z")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
}

func TestAskBareDatesCoverWholeDays(t *testing.T) {
	rt, query := mockQuery(t)
	mockSynthesis(t, rt).EXPECT().Synthesize(gomock.Any(), gomock.Any(), gomock.Any()).Return(proseAnswer, nil)
	query.EXPECT().FindDecision(gomock.Any(), services.FindDecisionRequest{
		Question: "why sqlite?",
		Since:    time.Date(2025, time.March, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2025, time.March, 31, 23, 59, 59, 0, time.UTC),
	}).Return(&entities.EvidenceBundle{Question: "why sqlite?"}, nil)

	res := run(t, rt, "ask", "why sqlite?", "--since", "2025-03-01", "--until", "2025-03-31")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
}

func TestAskRejectsAnUnparseableDate(t *testing.T) {
	rt, _ := mockQuery(t)

	res := run(t, rt, "ask", "why sqlite?", "--since", "last tuesday")
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitBadRequest)
	}
	if !strings.Contains(res.stderr, "--since") || !strings.Contains(res.stderr, "last tuesday") {
		t.Errorf("stderr = %q, want it to name the flag and the value", res.stderr)
	}
	if res.released {
		t.Error("the workspace was built for an invocation that could not run")
	}
}

func TestAskMapsServiceErrorsToExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{{
		name: "precondition keeps its remediation",
		err:  internalerror.NewPreconditionError("index was embedded with openai/text-embedding-3-large/3072 — run `lore sync --reembed`", nil),
		code: exitPrecondition,
	}, {
		name: "bad request",
		err:  internalerror.NewBadRequestError("question must not be empty", nil),
		code: exitBadRequest,
	}, {
		name: "not found",
		err:  internalerror.NewNotFoundError("no configuration at ./lore.yaml", nil),
		code: exitNotFound,
	}, {
		name: "internal",
		err:  internalerror.NewInternalError("cannot search the index", errUnclassified),
		code: exitInternal,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt, query := mockQuery(t)
			mockSynthesis(t, rt)
			query.EXPECT().FindDecision(gomock.Any(), gomock.Any()).Return(nil, c.err)

			res := run(t, rt, "ask", "why sqlite?")
			if res.exitCode != c.code {
				t.Errorf("exit = %d, want %d", res.exitCode, c.code)
			}
			if !strings.Contains(res.stderr, c.err.Error()) {
				t.Errorf("stderr = %q, want it to carry %q", res.stderr, c.err)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want nothing on failure", res.stdout)
			}
			if !res.released {
				t.Error("the workspace was not released after a failure")
			}
		})
	}
}
