package services_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	mock_llm "lore/internal/mocks/llm"
	"lore/internal/services"
)

const synthesisQuestion = "why did we choose option B instead of A?"

var errSynthesisProvider = errors.New("provider refused the request: 503 service unavailable")

type synthesisFixture struct {
	model *mock_llm.MockLLM
	svc   services.SynthesisService
}

func newSynthesisFixture(t *testing.T) synthesisFixture {
	t.Helper()

	model := mock_llm.NewMockLLM(gomock.NewController(t))

	return synthesisFixture{model: model, svc: services.NewSynthesisService(model)}
}

type capturedPrompt struct {
	system string
	user   string
}

func (f synthesisFixture) promptFor(answer string) *capturedPrompt {
	captured := new(capturedPrompt)
	f.model.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, system, user string) (string, error) {
			*captured = capturedPrompt{system: system, user: user}
			return answer, nil
		})

	return captured
}

func synthesisBundle() *entities.EvidenceBundle {
	at := time.Date(2025, time.March, 12, 9, 30, 0, 0, time.UTC)

	return &entities.EvidenceBundle{
		Question: synthesisQuestion,
		Anchor: entities.Anchor{
			Kind:  entities.AnchorQuery | entities.AnchorTimeWindow,
			Query: synthesisQuestion,
			Window: &entities.TimeWindow{
				From:       at.AddDate(0, 0, -30),
				To:         at.AddDate(0, 0, 30),
				Derivation: "event 'incident X' via INC-201",
				AnchoredBy: "jira:issue:INC-201",
			},
		},
		Nodes: []entities.EvidenceNode{
			{
				Doc: entities.DocumentMeta{
					ID:        "notion:page:decision-b",
					Source:    "notion",
					Type:      entities.DocTypePage,
					Title:     "Decision: option B for the ingest path",
					Author:    "alice",
					URL:       "https://notion.example.test/decision-b",
					CreatedAt: at,
				},
				Excerpt: "Option A needed a second queue; option B reuses the existing one.",
				Role:    entities.RoleSeed,
			},
			{
				Doc: entities.DocumentMeta{
					ID:        "github:pull_request:acme/lore#41",
					Source:    "github",
					Type:      entities.DocTypePR,
					Title:     "Implement the option B ingest path",
					Author:    "bob",
					URL:       "https://github.example.test/acme/lore/pull/41",
					CreatedAt: at.AddDate(0, 0, 3),
				},
				Excerpt: "Follows the decision page; drops the second queue.",
				Role:    entities.RoleLinkedChange,
			},
			{
				Doc: entities.DocumentMeta{
					ID:        "jira:issue:PROJ-4521",
					Source:    "jira",
					Type:      entities.DocTypeTicket,
					Title:     "Ingest path is dropping events under load",
					Author:    "carol",
					URL:       "https://jira.example.test/PROJ-4521",
					CreatedAt: at.AddDate(0, 0, 9),
				},
				Excerpt: "Reported after the option B rollout.",
				Role:    entities.RoleFollowUp,
			},
		},
		Chains: [][]entities.DocID{
			{"jira:issue:INC-201", "notion:page:decision-b", "github:pull_request:acme/lore#41"},
		},
		Gaps: []string{"trail ends at PROJ-4521; no linked follow-up"},
	}
}

func TestSynthesizeGroundsTheAnswerInTheNumberedEvidence(t *testing.T) {
	f := newSynthesisFixture(t)
	bundle := synthesisBundle()

	const prose = "Option B reused the existing queue [1], and the change landed in PR 41 [2]. " +
		"A load regression followed [3]; the trail stops there."
	prompt := f.promptFor(prose)

	got, err := f.svc.Synthesize(context.Background(), synthesisQuestion, bundle)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	if want := "Answer ONLY from the numbered evidence"; !strings.Contains(prompt.system, want) {
		t.Errorf("system instruction does not carry %q:\n%s", want, prompt.system)
	}
	if !strings.Contains(prompt.user, synthesisQuestion) {
		t.Errorf("prompt does not carry the question:\n%s", prompt.user)
	}
	for i, node := range bundle.Nodes {
		for _, want := range []string{node.Doc.Title, node.Doc.URL, node.Excerpt, node.Doc.Author} {
			if !strings.Contains(prompt.user, want) {
				t.Errorf("prompt does not carry node %d's %q", i+1, want)
			}
		}
	}
	for _, want := range []string{
		bundle.Gaps[0],
		"[1] -> [2]",
		"jira:issue:INC-201",
		bundle.Anchor.Window.Derivation,
	} {
		if !strings.Contains(prompt.user, want) {
			t.Errorf("prompt does not carry %q:\n%s", want, prompt.user)
		}
	}

	if !strings.HasPrefix(got, prose) {
		t.Errorf("answer = %q, want it to keep the model's prose verbatim", got)
	}
	wantSources := "**Sources**\n\n" +
		"1. Decision: option B for the ingest path — https://notion.example.test/decision-b\n" +
		"2. Implement the option B ingest path — https://github.example.test/acme/lore/pull/41\n" +
		"3. Ingest path is dropping events under load — https://jira.example.test/PROJ-4521\n"
	if !strings.HasSuffix(got, wantSources) {
		t.Errorf("answer does not end with the source list in node order:\n%s", got)
	}
}

func TestSynthesizeRejectsACitationOutsideTheEvidence(t *testing.T) {
	f := newSynthesisFixture(t)
	f.promptFor("Option B was cheaper [1], and the rollout was reviewed [7].")

	_, err := f.svc.Synthesize(context.Background(), synthesisQuestion, synthesisBundle())
	if err == nil {
		t.Fatal("Synthesize: want an error for a citation with no document")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindInternal {
		t.Errorf("kind = %s, want %s", got, internalerror.KindInternal)
	}
	if want := "the answer cites [7], but the evidence is numbered 1 to 3"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func TestSynthesizeRejectsAnAnswerWithNoCitationAtAll(t *testing.T) {
	f := newSynthesisFixture(t)
	f.promptFor("We picked option B because it was the cheaper path.")

	_, err := f.svc.Synthesize(context.Background(), synthesisQuestion, synthesisBundle())
	if err == nil {
		t.Fatal("Synthesize: want an error for ungrounded prose")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindInternal {
		t.Errorf("kind = %s, want %s", got, internalerror.KindInternal)
	}
	if want := "the answer cites none of the 3 evidence documents"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func TestSynthesizeWithoutAConfiguredLLMNamesTheRemedy(t *testing.T) {
	svc := services.NewSynthesisService(nil)

	got, err := svc.Synthesize(context.Background(), synthesisQuestion, synthesisBundle())
	if err == nil {
		t.Fatal("Synthesize: want an error naming the missing configuration")
	}
	if got != "" {
		t.Errorf("answer = %q, want no answer at all", got)
	}
	if kind := internalerror.KindOf(err); kind != internalerror.KindPrecondition {
		t.Errorf("kind = %s, want %s", kind, internalerror.KindPrecondition)
	}
	const want = "synthesis needs an LLM, and this workspace has no llm: block in lore.yaml — " +
		"add one naming the provider, the model and the api_key_env that holds its key"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestSynthesizePropagatesTheProviderFailure(t *testing.T) {
	f := newSynthesisFixture(t)
	f.model.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any()).Return("", errSynthesisProvider)

	_, err := f.svc.Synthesize(context.Background(), synthesisQuestion, synthesisBundle())
	if err == nil {
		t.Fatal("Synthesize: want the provider's failure")
	}
	if !errors.Is(err, errSynthesisProvider) {
		t.Errorf("error %v does not wrap the provider's failure", err)
	}
	if got := internalerror.KindOf(err); got != internalerror.KindInternal {
		t.Errorf("kind = %s, want %s", got, internalerror.KindInternal)
	}
	for _, leak := range []string{"Excerpt", "Option A needed a second queue", "Cite the evidence number"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks the prompt", err)
		}
	}
}

func TestSynthesizeAnswersFromGapsWhenThereIsNoEvidence(t *testing.T) {
	f := newSynthesisFixture(t)
	const prose = "The index holds nothing about option B; the only recorded gap is an unresolved event."
	prompt := f.promptFor(prose)

	bundle := &entities.EvidenceBundle{
		Question: synthesisQuestion,
		Anchor:   entities.Anchor{Kind: entities.AnchorQuery, Query: synthesisQuestion},
		Gaps:     []string{"could not resolve event 'incident X' to a time"},
	}

	got, err := f.svc.Synthesize(context.Background(), synthesisQuestion, bundle)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if got != prose {
		t.Errorf("answer = %q, want the prose alone with no source list", got)
	}
	if !strings.Contains(prompt.user, bundle.Gaps[0]) {
		t.Errorf("prompt does not carry the gap:\n%s", prompt.user)
	}
}

func TestSynthesizePassesTheCallersContextToTheProvider(t *testing.T) {
	f := newSynthesisFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f.model.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(passed context.Context, _, _ string) (string, error) {
			return "", passed.Err()
		})

	_, err := f.svc.Synthesize(ctx, synthesisQuestion, synthesisBundle())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the cancellation the caller's context carried", err)
	}
}

func TestSynthesizeRejectsMalformedCitations(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		want   string
	}{
		{
			name:   "padded number outside the evidence",
			answer: "Option B reused the queue [1], and a later review agreed [ 12 ].",
			want:   "the answer cites [12], but the evidence is numbered 1 to 3",
		},
		{
			name:   "citation smuggling a url as a markdown link",
			answer: "Option B reused the queue [1](https://notion.example.test/decision-b).",
			want:   "the answer wrote citation [1] as a markdown link",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newSynthesisFixture(t)
			f.promptFor(test.answer)

			_, err := f.svc.Synthesize(context.Background(), synthesisQuestion, synthesisBundle())
			if err == nil {
				t.Fatal("Synthesize: want an error for a citation the source list cannot back")
			}
			if got := internalerror.KindOf(err); got != internalerror.KindInternal {
				t.Errorf("kind = %s, want %s", got, internalerror.KindInternal)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %q, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestSynthesizeDescribesAWholeFileCodeAnchorWithoutALineSpan(t *testing.T) {
	f := newSynthesisFixture(t)
	prompt := f.promptFor("The file was renamed twice [1].")

	bundle := synthesisBundle()
	bundle.Anchor = entities.Anchor{
		Kind: entities.AnchorCodeSpan,
		Code: &entities.CodeAnchor{
			Repo:       "acme/lore",
			File:       "internal/auth/auth.go",
			BlamedSHAs: []string{"abc1234"},
		},
	}

	if _, err := f.svc.Synthesize(context.Background(), synthesisQuestion, bundle); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if !strings.Contains(prompt.user, "code acme/lore internal/auth/auth.go blamed on abc1234") {
		t.Errorf("prompt does not describe the whole-file anchor:\n%s", prompt.user)
	}
	if strings.Contains(prompt.user, ":0-0") {
		t.Errorf("prompt invents a line span for a whole-file anchor:\n%s", prompt.user)
	}
}
