package services_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/mocks/lore"
	mock_repositories "github.com/setthasit/Lore/internal/mocks/repositories"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/sdk"
)

const (
	whyTopK      = 4
	whyWalkDepth = 2
)

const (
	askOnlyMessage = "no repositories registered — code anchoring disabled for this workspace"
	whyFile        = "internal/auth/auth.go"
	whyRemote      = "github:acme/lore"
	whyPath        = "/home/dev/lore"
	otherPath      = "/home/dev/tools"
	whyAsked       = "why is the cold-start token check here?"
)

const (
	whyLineStart = 10
	whyLineEnd   = 13
)

const (
	whyShaA = "1111111111111111111111111111111111111111"
	whyShaB = "2222222222222222222222222222222222222222"
	whyShaC = "3333333333333333333333333333333333333333"
)

const (
	whyCommitAID lore.DocID = "github:commit:aaa"
	whyCommitBID lore.DocID = "github:commit:bbb"
	whyPRID      lore.DocID = "github:pr:42"
	whyIssueID   lore.DocID = "github:issue:7"
	whyPageID    lore.DocID = "notion:page:auth-design"
)

const whyDay = 24 * time.Hour

var errWhyStore = errors.New("index is on fire")

var whyVector = []float32{0.25, -0.5, 0.75}

var whyAt = time.Date(2025, 3, 12, 9, 0, 0, 0, time.UTC)

var (
	whyCommitAMeta = whyMeta(whyCommitAID, lore.DocTypeCommit, "tighten the cold-start token check", whyAt)
	whyCommitBMeta = whyMeta(whyCommitBID, lore.DocTypeCommit, "close the auth guard", whyAt.Add(whyDay))
	whyPRMeta      = whyMeta(whyPRID, lore.DocTypePR, "harden auth on cold start", whyAt.Add(-2*whyDay))
	whyIssueMeta   = whyMeta(whyIssueID, lore.DocTypeIssue, "auth times out on cold start", whyAt.Add(-5*whyDay))
	whyPageMeta    = whyMeta(whyPageID, lore.DocTypePage, "auth design", whyAt.Add(-10*whyDay))
)

var (
	whyPRTouchesCommitA    = whyEdge(whyPRID, whyCommitAID)
	whyIssueTouchesCommitB = whyEdge(whyIssueID, whyCommitBID)
)

// The blamed span is A,A,B,A: commit A owns lines the collapsed spans do not keep together.
var (
	whyFirstA  = whyBlamed(whyShaA, 10, 11, "\tif !token.Valid() {", "\t\treturn errUnauthorized")
	whyOnlyB   = whyBlamed(whyShaB, 12, 12, "\t}")
	whySecondA = whyBlamed(whyShaA, 13, 13, "\tlog.Warn(\"denied\")")
	whyOnlyC   = whyBlamed(whyShaC, whyLineStart, whyLineStart, "\tif !token.Valid() {")
)

var (
	oneRepo  = []services.CodeRepo{{Path: whyPath, Remote: whyRemote}}
	twoRepos = []services.CodeRepo{
		{Path: whyPath, Remote: whyRemote},
		{Path: otherPath},
	}
)

func whyMeta(id lore.DocID, docType lore.DocType, title string, createdAt time.Time) entities.DocumentMeta {
	return entities.DocumentMeta{
		ID:        id,
		Source:    "github",
		Type:      docType,
		Title:     title,
		Author:    "ada@example.test",
		URL:       "https://example.test/" + string(id),
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}

func whyEdge(src, dst lore.DocID) entities.Edge {
	return entities.Edge{Src: src, Dst: dst, Kind: entities.EdgeKindReferencesDoc, Confidence: 1}
}

func whyBlamed(sha string, start, end int, lines ...string) lore.BlameSpan {
	return lore.BlameSpan{
		SHA:       sha,
		LineStart: start,
		LineEnd:   end,
		Author:    "Ada Lovelace",
		Time:      whyAt,
		Lines:     lines,
	}
}

func whyHit(id lore.DocID, docType lore.DocType) entities.ChunkHit {
	return entities.ChunkHit{
		Chunk: entities.Chunk{
			DocID:   id,
			Text:    string(id) + " excerpt",
			Source:  "github",
			DocType: docType,
		},
		Score: -2.5,
	}
}

func whyCode(spans ...lore.BlameSpan) string {
	var lines []string
	for _, span := range spans {
		lines = append(lines, span.Lines...)
	}

	return strings.Join(lines, "\n")
}

func whyEmbedText(parts ...string) string {
	return strings.Join(parts, "\n\n")
}

func whyDefaultQuestion(start, end int) string {
	return fmt.Sprintf("why does %s:%d-%d exist in its current form", whyFile, start, end)
}

func whyRequest() services.WhyRequest {
	return services.WhyRequest{File: whyFile, LineStart: whyLineStart, LineEnd: whyLineEnd, Question: whyAsked}
}

func whyUnsyncedGap(sha string) string {
	return "trail ends at commit " + sha[:12] + ", not synced from a source"
}

type whyFixture struct {
	store *mock_repositories.MockIndexStore
	emb   *mock_lore.MockEmbedder
	git   *mock_lore.MockCodeRepo
	svc   services.WhyService
}

func newWhyFixture(t *testing.T) whyFixture {
	t.Helper()

	ctrl := gomock.NewController(t)
	store := mock_repositories.NewMockIndexStore(ctrl)
	emb := mock_lore.NewMockEmbedder(ctrl)
	git := mock_lore.NewMockCodeRepo(ctrl)
	cfg := services.QueryConfig{TopK: whyTopK, WalkDepth: whyWalkDepth}
	repos := []services.CodeRepo{{Path: whyPath, Remote: whyRemote, Git: git}}

	return whyFixture{
		store: store,
		emb:   emb,
		git:   git,
		svc:   services.NewWhyService(store, emb, cfg, repos),
	}
}

func (f whyFixture) expectBlame(start, end int, spans ...lore.BlameSpan) {
	f.git.EXPECT().HasFileAtHEAD(gomock.Any(), whyFile).Return(true, nil)
	f.git.EXPECT().Blame(gomock.Any(), whyFile, start, end).Return(spans, nil)
}

func (f whyFixture) expectResolve(sha string, candidates ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().ResolveRef(gomock.Any(), sha).Return(candidates, nil)
}

func (f whyFixture) expectMetas(ids []lore.DocID, metas ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().DocumentsByID(gomock.Any(), ids).Return(metas, nil)
}

func (f whyFixture) expectNeighbors(ids []lore.DocID, edges ...entities.Edge) *gomock.Call {
	return f.store.EXPECT().Neighbors(gomock.Any(), ids, nil, entities.DirBoth).Return(edges, nil)
}

func (f whyFixture) expectSearch(text string, hits ...entities.ChunkHit) {
	unfiltered := gomock.Eq(entities.Filters{})
	f.emb.EXPECT().Embed(gomock.Any(), []string{text}).Return([][]float32{whyVector}, nil)
	f.store.EXPECT().SearchLexical(gomock.Any(), text, unfiltered, whyTopK).Return(hits, nil)
	f.store.EXPECT().SearchVector(gomock.Any(), whyVector, unfiltered, whyTopK).Return(nil, nil)
}

func whyRoles(nodes []entities.EvidenceNode) []string {
	roles := make([]string, len(nodes))
	for i, node := range nodes {
		roles[i] = node.Role
	}

	return roles
}

func assertWhyNodes(t *testing.T, nodes []entities.EvidenceNode, want []lore.DocID) {
	t.Helper()

	if got := nodeIDs(nodes); !slices.Equal(got, want) {
		t.Fatalf("Nodes = %v, want %v", got, want)
	}
}

func assertWhyRoles(t *testing.T, nodes []entities.EvidenceNode, want []string) {
	t.Helper()

	if got := whyRoles(nodes); !slices.Equal(got, want) {
		t.Errorf("roles = %q, want %q", got, want)
	}
}

func assertWhyChains(t *testing.T, got, want [][]lore.DocID) {
	t.Helper()

	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("Chains = %v, want %v", got, want)
	}
}

func assertWhyAnchor(t *testing.T, anchor entities.Anchor, end int, shas []string) {
	t.Helper()

	if anchor.Kind != entities.AnchorCodeSpan {
		t.Errorf("Anchor.Kind = %d, want %d", anchor.Kind, entities.AnchorCodeSpan)
	}
	if anchor.Doc != nil || anchor.Window != nil || anchor.Query != "" {
		t.Errorf("Anchor carries a non-code grounding: %+v", anchor)
	}
	want := entities.CodeAnchor{
		Repo:       whyRemote,
		File:       whyFile,
		LineStart:  whyLineStart,
		LineEnd:    end,
		BlamedSHAs: shas,
	}
	if anchor.Code == nil {
		t.Fatalf("Anchor.Code = nil, want %+v", want)
	}
	if anchor.Code.Repo != want.Repo || anchor.Code.File != want.File ||
		anchor.Code.LineStart != want.LineStart || anchor.Code.LineEnd != want.LineEnd ||
		!slices.Equal(anchor.Code.BlamedSHAs, want.BlamedSHAs) {
		t.Errorf("Anchor.Code = %+v, want %+v", *anchor.Code, want)
	}
}

func TestWhyValidationOrder(t *testing.T) {
	tests := []struct {
		name     string
		repos    []services.CodeRepo
		req      services.WhyRequest
		kind     internalerror.Kind
		message  string
		contains []string
	}{
		{
			name:    "no repositories registered refuses code anchoring",
			req:     services.WhyRequest{File: whyFile, LineStart: 10, LineEnd: 20},
			kind:    internalerror.KindPrecondition,
			message: askOnlyMessage,
		},
		{
			name:    "no repositories outranks every other complaint",
			req:     services.WhyRequest{LineStart: -3, LineEnd: -9},
			kind:    internalerror.KindPrecondition,
			message: askOnlyMessage,
		},
		{
			name:     "a blank file is rejected",
			repos:    oneRepo,
			req:      services.WhyRequest{File: "   ", LineStart: 10, LineEnd: 20},
			kind:     internalerror.KindBadRequest,
			contains: []string{"file"},
		},
		{
			name:     "a zero line_start is rejected",
			repos:    oneRepo,
			req:      services.WhyRequest{File: whyFile, LineStart: 0, LineEnd: 20},
			kind:     internalerror.KindBadRequest,
			contains: []string{"line_start 0"},
		},
		{
			name:     "a negative line_start is rejected",
			repos:    oneRepo,
			req:      services.WhyRequest{File: whyFile, LineStart: -3, LineEnd: 20},
			kind:     internalerror.KindBadRequest,
			contains: []string{"line_start -3"},
		},
		{
			name:     "a line_end before line_start is rejected",
			repos:    oneRepo,
			req:      services.WhyRequest{File: whyFile, LineStart: 40, LineEnd: 12},
			kind:     internalerror.KindBadRequest,
			contains: []string{"40-12"},
		},
		{
			name:     "an unregistered repo names the request and the registered repos",
			repos:    twoRepos,
			req:      services.WhyRequest{Repo: "github:acme/other", File: whyFile, LineStart: 10, LineEnd: 20},
			kind:     internalerror.KindNotFound,
			contains: []string{"github:acme/other", whyRemote, otherPath},
		},
		{
			name:     "an unregistered repo outranks an invalid span",
			repos:    twoRepos,
			req:      services.WhyRequest{Repo: "github:acme/other", File: whyFile, LineStart: 40, LineEnd: 12},
			kind:     internalerror.KindNotFound,
			contains: []string{"github:acme/other"},
		},
		{
			name:     "an omitted repo with several registered asks for one",
			repos:    twoRepos,
			req:      services.WhyRequest{File: whyFile, LineStart: 10, LineEnd: 20},
			kind:     internalerror.KindBadRequest,
			contains: []string{"repo must name one", whyRemote, otherPath},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := services.NewWhyService(nil, nil, services.QueryConfig{}, tt.repos)

			bundle, err := svc.Why(context.Background(), tt.req)

			if bundle != nil {
				t.Errorf("bundle = %+v, want none alongside a refusal", bundle)
			}
			if got := internalerror.KindOf(err); got != tt.kind {
				t.Fatalf("kind = %s, want %s (error %v)", got, tt.kind, err)
			}
			if tt.message != "" && err.Error() != tt.message {
				t.Errorf("message = %q, want %q", err.Error(), tt.message)
			}
			for _, want := range tt.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message = %q, want it to name %q", err.Error(), want)
				}
			}
		})
	}
}

func TestWhyChainsBlamedCommitsToTheirDiscussion(t *testing.T) {
	t.Parallel()

	commitACode := whyCode(whyFirstA, whySecondA)

	f := newWhyFixture(t)
	f.expectBlame(whyLineStart, whyLineEnd, whyFirstA, whyOnlyB, whySecondA)
	f.expectResolve(whyShaA, whyCommitAMeta)
	f.expectResolve(whyShaB, whyCommitBMeta)

	seeds := []lore.DocID{whyCommitAID, whyCommitBID}
	f.expectMetas(seeds, whyCommitAMeta, whyCommitBMeta)
	f.expectNeighbors(seeds, whyPRTouchesCommitA, whyIssueTouchesCommitB)
	f.expectMetas([]lore.DocID{whyPRID, whyIssueID}, whyPRMeta, whyIssueMeta)
	f.expectNeighbors([]lore.DocID{whyPRID, whyIssueID})

	// The blamed code is grouped per commit, not left in collapsed-span order.
	f.expectSearch(
		whyEmbedText(whyAsked,
			commitACode+"\n"+whyCode(whyOnlyB),
			whyCommitAMeta.Title+"\n"+whyCommitBMeta.Title),
		whyHit(whyPageID, lore.DocTypePage))
	f.expectMetas([]lore.DocID{whyPageID}, whyPageMeta)

	bundle, err := f.svc.Why(context.Background(), whyRequest())
	if err != nil {
		t.Fatalf("Why: %v", err)
	}

	assertWhyNodes(t, bundle.Nodes, []lore.DocID{
		whyCommitBID, whyCommitAID, whyPageID, whyPRID, whyIssueID,
	})
	assertWhyRoles(t, bundle.Nodes, []string{
		entities.RoleBlamedCommit,
		entities.RoleBlamedCommit,
		entities.RoleSemanticMatch,
		entities.RoleLinkedChange,
		entities.RoleLinkedTicket,
	})

	blamedA := bundle.Nodes[1]
	if blamedA.Doc != whyCommitAMeta || blamedA.Excerpt != commitACode || blamedA.Via != nil {
		t.Errorf("blamed node = %+v, want %+v excerpted with %q and no edges", blamedA, whyCommitAMeta, commitACode)
	}
	assertScore(t, "blamed commit", blamedA.Score, 1)

	pr := bundle.Nodes[3]
	if !slices.Equal(pr.Via, []entities.Edge{whyPRTouchesCommitA}) {
		t.Errorf("linked change Via = %+v, want %+v", pr.Via, whyPRTouchesCommitA)
	}
	assertScore(t, "linked change", pr.Score, 0.6)

	match := bundle.Nodes[2]
	if match.Excerpt != string(whyPageID)+" excerpt" {
		t.Errorf("semantic node excerpt = %q, want its retrieval excerpt", match.Excerpt)
	}
	assertScore(t, "semantic match", match.Score, 1)

	if bundle.Question != whyAsked {
		t.Errorf("Question = %q, want the asked question %q", bundle.Question, whyAsked)
	}
	assertWhyAnchor(t, bundle.Anchor, whyLineEnd, []string{whyShaA, whyShaB})
	assertWhyChains(t, bundle.Chains, [][]lore.DocID{
		{whyCommitAID, whyPRID},
		{whyCommitBID, whyIssueID},
	})
	assertGaps(t, bundle.Gaps, nil)
}

func TestWhyReportsAnUnsyncedCommitAsAGapAndKeepsTheRest(t *testing.T) {
	t.Parallel()

	f := newWhyFixture(t)
	f.expectBlame(whyLineStart, whyLineEnd, whyFirstA, whyOnlyC)
	f.expectResolve(whyShaA, whyCommitAMeta)
	// A non-commit candidate is not a blamed commit, so this SHA still ends the trail.
	f.expectResolve(whyShaC, whyPRMeta)

	f.expectMetas([]lore.DocID{whyCommitAID}, whyCommitAMeta)
	f.expectNeighbors([]lore.DocID{whyCommitAID}, whyPRTouchesCommitA)
	f.expectMetas([]lore.DocID{whyPRID}, whyPRMeta)
	f.expectNeighbors([]lore.DocID{whyPRID})

	f.expectSearch(whyEmbedText(whyAsked,
		whyCode(whyFirstA)+"\n"+whyCode(whyOnlyC),
		whyCommitAMeta.Title))

	bundle, err := f.svc.Why(context.Background(), whyRequest())
	if err != nil {
		t.Fatalf("Why: %v", err)
	}

	assertWhyNodes(t, bundle.Nodes, []lore.DocID{whyCommitAID, whyPRID})
	assertWhyChains(t, bundle.Chains, [][]lore.DocID{{whyCommitAID, whyPRID}})
	assertGaps(t, bundle.Gaps, []string{whyUnsyncedGap(whyShaC)})
	assertWhyAnchor(t, bundle.Anchor, whyLineEnd, []string{whyShaA, whyShaC})
}

func TestWhyReturnsGapsWithoutEvidenceWhenNoCommitIsSynced(t *testing.T) {
	t.Parallel()

	f := newWhyFixture(t)
	f.expectBlame(whyLineStart, whyLineEnd, whyFirstA, whyOnlyB)
	f.expectResolve(whyShaA)
	f.expectResolve(whyShaB)
	f.expectSearch(whyEmbedText(whyAsked, whyCode(whyFirstA)+"\n"+whyCode(whyOnlyB)))

	bundle, err := f.svc.Why(context.Background(), whyRequest())
	if err != nil {
		t.Fatalf("Why with nothing synced: %v", err)
	}

	if len(bundle.Nodes) != 0 {
		t.Errorf("Nodes = %v, want none", nodeIDs(bundle.Nodes))
	}
	if bundle.Chains != nil {
		t.Errorf("Chains = %v, want none", bundle.Chains)
	}
	assertGaps(t, bundle.Gaps, []string{whyUnsyncedGap(whyShaA), whyUnsyncedGap(whyShaB)})
	assertWhyAnchor(t, bundle.Anchor, whyLineEnd, []string{whyShaA, whyShaB})
}

func TestWhyReportsAStandaloneBlamedCommitAsAGap(t *testing.T) {
	t.Parallel()

	f := newWhyFixture(t)
	f.expectBlame(whyLineStart, whyLineEnd, whyFirstA)
	f.expectResolve(whyShaA, whyCommitAMeta)
	f.expectMetas([]lore.DocID{whyCommitAID}, whyCommitAMeta)
	f.expectNeighbors([]lore.DocID{whyCommitAID})
	f.expectSearch(whyEmbedText(whyAsked, whyCode(whyFirstA), whyCommitAMeta.Title))

	bundle, err := f.svc.Why(context.Background(), whyRequest())
	if err != nil {
		t.Fatalf("Why: %v", err)
	}

	assertWhyNodes(t, bundle.Nodes, []lore.DocID{whyCommitAID})
	want := whyCommitAMeta.Title + " (" + string(whyCommitAID) + ") stands alone; no linked discussion"
	assertGaps(t, bundle.Gaps, []string{want})
}

func TestWhyRefusesAFileAbsentAtHEADWithoutBlaming(t *testing.T) {
	t.Parallel()

	// Only HasFileAtHEAD is expected: an absent file is never blamed.
	f := newWhyFixture(t)
	f.git.EXPECT().HasFileAtHEAD(gomock.Any(), whyFile).Return(false, nil)

	bundle, err := f.svc.Why(context.Background(), whyRequest())

	if bundle != nil {
		t.Errorf("bundle = %+v, want none for an absent file", bundle)
	}
	if got := internalerror.KindOf(err); got != internalerror.KindNotFound {
		t.Fatalf("kind = %s, want %s (error %v)", got, internalerror.KindNotFound, err)
	}
	for _, want := range []string{whyFile, whyRemote} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message = %q, want it to name %q", err.Error(), want)
		}
	}
}

func TestWhyDefaultsTheQuestionToTheBlamedSpan(t *testing.T) {
	t.Parallel()

	want := whyDefaultQuestion(whyLineStart, whyLineEnd)

	f := newWhyFixture(t)
	f.expectBlame(whyLineStart, whyLineEnd, whyOnlyC)
	f.expectResolve(whyShaC)
	f.expectSearch(whyEmbedText(want, whyCode(whyOnlyC)))

	bundle, err := f.svc.Why(context.Background(), services.WhyRequest{
		File:      whyFile,
		LineStart: whyLineStart,
		LineEnd:   whyLineEnd,
		Question:  "  \t",
	})
	if err != nil {
		t.Fatalf("Why: %v", err)
	}
	if bundle.Question != want {
		t.Errorf("Question = %q, want %q", bundle.Question, want)
	}
}

func TestWhyBlamesASingleLineForAZeroLineEnd(t *testing.T) {
	t.Parallel()

	f := newWhyFixture(t)
	f.expectBlame(whyLineStart, whyLineStart, whyOnlyC)
	f.expectResolve(whyShaC)
	f.expectSearch(whyEmbedText(whyDefaultQuestion(whyLineStart, whyLineStart), whyCode(whyOnlyC)))

	bundle, err := f.svc.Why(context.Background(), services.WhyRequest{File: whyFile, LineStart: whyLineStart})
	if err != nil {
		t.Fatalf("Why: %v", err)
	}
	assertWhyAnchor(t, bundle.Anchor, whyLineStart, []string{whyShaC})
}

func TestWhyCitesADoublyFoundDocumentOnce(t *testing.T) {
	t.Parallel()

	f := newWhyFixture(t)
	f.expectBlame(whyLineStart, whyLineEnd, whyFirstA)
	f.expectResolve(whyShaA, whyCommitAMeta)
	f.expectMetas([]lore.DocID{whyCommitAID}, whyCommitAMeta)
	f.expectNeighbors([]lore.DocID{whyCommitAID}, whyPRTouchesCommitA)
	f.expectMetas([]lore.DocID{whyPRID}, whyPRMeta)
	f.expectNeighbors([]lore.DocID{whyPRID})

	f.expectSearch(whyEmbedText(whyAsked, whyCode(whyFirstA), whyCommitAMeta.Title),
		whyHit(whyPRID, lore.DocTypePR))
	f.expectMetas([]lore.DocID{whyPRID}, whyPRMeta)

	bundle, err := f.svc.Why(context.Background(), whyRequest())
	if err != nil {
		t.Fatalf("Why: %v", err)
	}

	assertWhyNodes(t, bundle.Nodes, []lore.DocID{whyCommitAID, whyPRID})
	assertWhyRoles(t, bundle.Nodes, []string{entities.RoleBlamedCommit, entities.RoleLinkedChange})
	if bundle.Nodes[1].Excerpt != "" {
		t.Errorf("walked node excerpt = %q, want the walk's node, not the retrieval hit", bundle.Nodes[1].Excerpt)
	}
}

func TestWhyBlamesTheCloneTheRequestNames(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	named := mock_lore.NewMockCodeRepo(ctrl)
	// The first clone is given no expectations: naming the second must not blame it.
	repos := []services.CodeRepo{
		{Path: whyPath, Remote: whyRemote, Git: mock_lore.NewMockCodeRepo(ctrl)},
		{Path: otherPath, Git: named},
	}
	svc := services.NewWhyService(
		mock_repositories.NewMockIndexStore(ctrl),
		mock_lore.NewMockEmbedder(ctrl),
		services.QueryConfig{},
		repos,
	)

	named.EXPECT().HasFileAtHEAD(gomock.Any(), whyFile).Return(false, nil)

	_, err := svc.Why(context.Background(), services.WhyRequest{
		Repo:      otherPath,
		File:      whyFile,
		LineStart: whyLineStart,
	})

	if internalerror.KindOf(err) != internalerror.KindNotFound || !strings.Contains(err.Error(), otherPath) {
		t.Fatalf("error = %v, want a not-found naming the clone %q", err, otherPath)
	}
}

func TestWhyKeepsTheReposItWasConstructedWith(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	git := mock_lore.NewMockCodeRepo(ctrl)
	repos := []services.CodeRepo{{Path: whyPath, Remote: whyRemote, Git: git}}
	svc := services.NewWhyService(
		mock_repositories.NewMockIndexStore(ctrl),
		mock_lore.NewMockEmbedder(ctrl),
		services.QueryConfig{},
		repos,
	)
	repos[0] = services.CodeRepo{Path: otherPath, Remote: "github:acme/rewritten"}

	git.EXPECT().HasFileAtHEAD(gomock.Any(), whyFile).Return(false, nil)

	_, err := svc.Why(context.Background(), services.WhyRequest{
		Repo:      whyRemote,
		File:      whyFile,
		LineStart: whyLineStart,
	})

	if internalerror.KindOf(err) != internalerror.KindNotFound || !strings.Contains(err.Error(), whyRemote) {
		t.Fatalf("error = %v, want the repo registered at construction to still be blamed", err)
	}
}

func TestWhySurfacesStoreFailures(t *testing.T) {
	t.Parallel()

	f := newWhyFixture(t)
	f.expectBlame(whyLineStart, whyLineEnd, whyFirstA)
	f.store.EXPECT().ResolveRef(gomock.Any(), whyShaA).Return(nil, errWhyStore)

	bundle, err := f.svc.Why(context.Background(), whyRequest())

	if bundle != nil {
		t.Errorf("bundle = %+v, want none alongside a failure", bundle)
	}
	if got := internalerror.KindOf(err); got != internalerror.KindInternal {
		t.Fatalf("kind = %s, want %s (error %v)", got, internalerror.KindInternal, err)
	}
	if !strings.Contains(err.Error(), whyShaA[:12]) {
		t.Errorf("message = %q, want it to name the blamed commit", err.Error())
	}
	if !errors.Is(err, errWhyStore) {
		t.Errorf("error %v does not wrap %v", err, errWhyStore)
	}
}
