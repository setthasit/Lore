package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	mock_services "github.com/setthasit/Lore/internal/mocks/services"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/transport/mcp"
	"github.com/setthasit/Lore/sdk"
)

const proseAnswer = "SQLite won because it ships everywhere and needs no server [1]; " +
	"postgres with pgvector was the alternative [2].\n\n" +
	"**Sources**\n" +
	"[1] https://github.com/acme/lore/pull/12\n" +
	"[2] https://notion.so/design/storage"

var errUnclassified = errors.New("the disk caught fire")

func fxLikeWrap(err error) error {
	return fmt.Errorf(`could not build arguments for function "github.com/setthasit/Lore/internal/di".newIndexStore: %w`, err)
}

type result struct {
	stdout   string
	stderr   string
	exitCode int
	released bool
	modules  []fx.Option
}

func run(t *testing.T, rt *Runtime, args ...string) result {
	t.Helper()

	return runWithInput(t, rt, "", args...)
}

func runWithInput(t *testing.T, rt *Runtime, stdin string, args ...string) result {
	t.Helper()

	var out, errOut bytes.Buffer
	res := result{}

	resolve := func(_ context.Context, _ string, modules ...fx.Option) (*Runtime, func() error, error) {
		if rt.Config == nil {
			rt.Config = new(config.Config)
		}
		res.modules = modules
		return rt, func() error { res.released = true; return nil }, nil
	}

	root := newRootCommand(resolve, registry.New(lore.Host{}))
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)

	err := root.ExecuteContext(context.Background())
	if err != nil {
		res.exitCode = report(&errOut, err)
	}
	res.stdout, res.stderr = out.String(), errOut.String()
	return res
}

func mockQuery(t *testing.T) (*Runtime, *mock_services.MockQueryService) {
	t.Helper()

	query := mock_services.NewMockQueryService(gomock.NewController(t))
	return &Runtime{Query: query}, query
}

func mockWhy(t *testing.T) (*Runtime, *mock_services.MockWhyService) {
	t.Helper()

	why := mock_services.NewMockWhyService(gomock.NewController(t))
	return &Runtime{Why: why}, why
}

func mockTrace(t *testing.T) (*Runtime, *mock_services.MockTraceService) {
	t.Helper()

	trace := mock_services.NewMockTraceService(gomock.NewController(t))
	return &Runtime{Trace: trace}, trace
}

func mockImpact(t *testing.T) (*Runtime, *mock_services.MockImpactService) {
	t.Helper()

	impact := mock_services.NewMockImpactService(gomock.NewController(t))
	return &Runtime{Impact: impact}, impact
}

func mockHistory(t *testing.T) (*Runtime, *mock_services.MockHistoryService) {
	t.Helper()

	history := mock_services.NewMockHistoryService(gomock.NewController(t))
	return &Runtime{History: history}, history
}

func mockSynthesis(t *testing.T, rt *Runtime) *mock_services.MockSynthesisService {
	t.Helper()

	synthesis := mock_services.NewMockSynthesisService(gomock.NewController(t))
	rt.Synthesis = synthesis
	return synthesis
}

func wantProse(t *testing.T, res result) {
	t.Helper()

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if res.stdout != proseAnswer+"\n" {
		t.Errorf("stdout = %q, want the synthesized answer alone", res.stdout)
	}
}

func wantBundleJSON(t *testing.T, res result, bundle *entities.EvidenceBundle) {
	t.Helper()

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	want, err := mcp.EncodeBundle(bundle)
	if err != nil {
		t.Fatalf("EncodeBundle: %v", err)
	}
	if res.stdout != string(want)+"\n" {
		t.Errorf("stdout is not the canonical encoding alone\n got: %s\nwant: %s\n", res.stdout, want)
	}
}

var (
	anchorDoc = entities.DocumentMeta{
		ID:        lore.NewDocID("notion", lore.DocTypePage, "design/storage"),
		Source:    "notion",
		Type:      lore.DocTypePage,
		Title:     "Storage design",
		Author:    "arch@example.test",
		URL:       "https://notion.so/design/storage",
		CreatedAt: time.Date(2025, time.March, 10, 9, 30, 0, 0, time.UTC),
	}
	followUpDoc = entities.DocumentMeta{
		ID:        lore.NewDocID("github", lore.DocTypePR, "12"),
		Source:    "github",
		Type:      lore.DocTypePR,
		Title:     "Index on SQLite, not Postgres",
		Author:    "dev@example.test",
		URL:       "https://github.com/acme/lore/pull/12",
		CreatedAt: time.Date(2025, time.March, 12, 14, 0, 0, 0, time.UTC),
	}
)

func timelineBundle(question string) *entities.EvidenceBundle {
	return &entities.EvidenceBundle{
		Question: question,
		Anchor:   documentAnchor(),
		Nodes: []entities.EvidenceNode{{
			Doc:     anchorDoc,
			Excerpt: "postgres with pgvector was the alternative",
			Role:    entities.RoleSeed,
			Score:   1,
		}, {
			Doc:     followUpDoc,
			Excerpt: "sqlite ships everywhere and needs no server",
			Role:    entities.RoleFollowUp,
			Score:   0.62,
		}},
		Chains: [][]lore.DocID{{anchorDoc.ID, followUpDoc.ID}},
		Gaps:   []string{"trail ends at PROJ-4521; no linked follow-up"},
	}
}

func documentAnchor() entities.Anchor {
	return entities.Anchor{
		Kind: entities.AnchorDocument,
		Doc: &entities.DocRef{
			ID:        anchorDoc.ID,
			Title:     anchorDoc.Title,
			URL:       anchorDoc.URL,
			CreatedAt: anchorDoc.CreatedAt,
		},
	}
}

func TestReportMapsKindsToExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"nil", nil, exitOK},
		{"bad request", internalerror.NewBadRequestError("--since is not a date", nil), exitBadRequest},
		{"precondition", internalerror.NewPreconditionError("embedder identity mismatch — run `lore sync --reembed`", nil), exitPrecondition},
		{"not found", internalerror.NewNotFoundError("no configuration at ./lore.yaml", nil), exitNotFound},
		{"internal", internalerror.NewInternalError("cannot open the workspace index", nil), exitInternal},
		{"unclassified", errUnclassified, exitInternal},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := report(&stderr, c.err); got != c.code {
				t.Errorf("report = %d, want %d", got, c.code)
			}
			if c.err == nil {
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want nothing", stderr.String())
				}
				return
			}
			if !strings.Contains(stderr.String(), c.err.Error()) {
				t.Errorf("stderr = %q, want it to carry %q", stderr.String(), c.err)
			}
		})
	}
}

func TestReportPrintsTheClassifiedMessageOnly(t *testing.T) {
	// The cause is what a caller-facing kind must not print: Message already says everything actionable.
	wrapped := fxLikeWrap(internalerror.NewPreconditionError("another process holds the sync lock", errUnclassified))

	var stderr bytes.Buffer
	if got := report(&stderr, wrapped); got != exitPrecondition {
		t.Errorf("report = %d, want %d", got, exitPrecondition)
	}
	if got := stderr.String(); got != "lore: another process holds the sync lock\n" {
		t.Errorf("stderr = %q, want the classified message alone", got)
	}
}

func TestMalformedInvocationsAreBadRequests(t *testing.T) {
	cases := [][]string{
		{"ask"},
		{"ask", "why sqlite?", "and?"},
		{"status", "--nonexistent"},
		{"status", "extra"},
		{"frobnicate"},
	}

	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			res := run(t, &Runtime{}, args...)
			if res.exitCode != exitBadRequest {
				t.Errorf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
			}
			if res.released {
				t.Error("the workspace was built for an invocation that could not run")
			}
		})
	}
}

func TestHelpSucceeds(t *testing.T) {
	for _, args := range [][]string{
		{"--help"}, {"ask", "--help"}, {"why", "--help"}, {"trace", "--help"}, {"impact", "--help"},
		{"history", "--help"}, {"init", "--help"},
	} {
		res := run(t, nil, args...)
		if res.exitCode != exitOK {
			t.Errorf("%v: exit = %d, stderr = %q", args, res.exitCode, res.stderr)
		}
		if res.stdout == "" {
			t.Errorf("%v: printed no help", args)
		}
	}
}

func TestExplainableVerbsDocumentTheFlagAndItsLLMRequirement(t *testing.T) {
	for _, verb := range []string{"why", "trace", "impact", "history"} {
		res := run(t, nil, verb, "--help")
		if res.exitCode != exitOK {
			t.Fatalf("%s --help: exit = %d, stderr = %q", verb, res.exitCode, res.stderr)
		}
		for _, want := range []string{"--explain", "prose", "llm: block in lore.yaml"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("%s --help does not mention %q\n--- help ---\n%s", verb, want, res.stdout)
			}
		}
	}
}

func TestRootHelpListsTheQueryCommands(t *testing.T) {
	res := run(t, nil, "--help")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	for _, want := range []string{"ask", "why", "trace", "impact", "history"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("help does not list %q\n--- help ---\n%s", want, res.stdout)
		}
	}
}
