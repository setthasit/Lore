package services_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/mocks/lore"
	mock_repositories "github.com/setthasit/Lore/internal/mocks/repositories"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/sdk"
)

const (
	linkSlug    = "acme/lore"
	linkFullSHA = "9f1a2b3c4d5e6f708192a3b4c5d6e7f80912a3b4"
	linkPageURL = "https://notion.so/design/retrieval"
	linkOldURL  = "https://notion.so/design/retrieval-v1"
	linkTicket  = "PROJ-123"
	linkFile    = "internal/auth/auth.go"
	linkClone   = "/clones/lore"
)

// Mirrors the resolver's own cap on how much of one path's history it takes.
const linkPathCommitCap = 50

var (
	linkCommitID  = lore.NewDocID("github", lore.DocTypeCommit, linkSlug+"/commit/"+linkFullSHA)
	linkPRID      = lore.NewDocID("github", lore.DocTypePR, linkSlug+"/pull/12")
	linkIssueID   = lore.NewDocID("github", lore.DocTypeIssue, linkSlug+"/issues/9")
	linkCommentID = lore.NewDocID("github", lore.DocTypeIssueComment, linkSlug+"/issues/9#c1")
	linkPageID    = lore.NewDocID("notion", lore.DocTypePage, "design/retrieval")
	linkOldPageID = lore.NewDocID("notion", lore.DocTypePage, "design/retrieval-v1")
	linkTicketID  = lore.NewDocID("jira", lore.DocTypeTicket, linkTicket)
)

var (
	errLinkStore = errors.New("store is on fire")
	errLinkGit   = errors.New("clone is unreadable")
)

type linkMocks struct {
	store *mock_repositories.MockIndexStore
	git   *mock_lore.MockCodeRepo
}

func newLinkMocks(t *testing.T) linkMocks {
	t.Helper()

	ctrl := gomock.NewController(t)

	return linkMocks{
		store: mock_repositories.NewMockIndexStore(ctrl),
		git:   mock_lore.NewMockCodeRepo(ctrl),
	}
}

func (m linkMocks) resolver() services.LinkResolver {
	return services.NewLinkResolver(m.store, []services.CodeRepo{{Path: linkClone, Git: m.git}})
}

func (m linkMocks) expectLog(shas ...string) {
	m.git.EXPECT().HasFileAtHEAD(gomock.Any(), linkFile).Return(true, nil)

	log := make([]lore.CommitRef, len(shas))
	for i, sha := range shas {
		log[i] = lore.CommitRef{SHA: sha}
	}
	m.git.EXPECT().Log(gomock.Any(), linkFile).Return(log, nil)
}

func (m linkMocks) expectCommit(sha string) *gomock.Call {
	return m.store.EXPECT().ResolveRef(gomock.Any(), sha).
		Return([]entities.DocumentMeta{linkMeta(linkCommitDocID(sha), lore.DocTypeCommit)}, nil)
}

func linkPathSHA(n int) string { return fmt.Sprintf("%040x", n) }

func linkCommitDocID(sha string) lore.DocID {
	return lore.NewDocID("github", lore.DocTypeCommit, linkSlug+"/commit/"+sha)
}

func linkPathDoc(id lore.DocID, docType lore.DocType, body string) lore.Document {
	return linkDoc(id, docType, body, lore.RawRef{Kind: lore.RefKindFilePath, Value: linkFile})
}

func linkPathEdge(source lore.DocID, sha string) entities.Edge {
	return entities.Edge{
		Src: source, Dst: linkCommitDocID(sha),
		Kind: entities.EdgeKindMentionsPath, Confidence: 0.7,
	}
}

func linkDoc(id lore.DocID, docType lore.DocType, body string, refs ...lore.RawRef) lore.Document {
	return lore.Document{
		ID:     id,
		Source: "github",
		Type:   docType,
		Title:  "Resolve refs after the batch commits",
		Body:   body,
		Refs:   refs,
	}
}

func linkMeta(id lore.DocID, docType lore.DocType) entities.DocumentMeta {
	return entities.DocumentMeta{ID: id, Type: docType}
}

func linkStored(doc lore.Document) lore.Document {
	doc.Refs = nil

	return doc
}

func linkOnly(doc lore.Document) []entities.PendingRef {
	return []entities.PendingRef{{SourceDoc: doc.ID, Ref: doc.Refs[0]}}
}

func TestLinkWritesTheEdgeEachRuleDictates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		source     lore.Document
		candidates []entities.DocumentMeta
		want       entities.Edge
	}{
		{
			name: "a commit names the pull request that carried it",
			source: linkDoc(linkCommitID, lore.DocTypeCommit, "fix the resolver",
				lore.RawRef{Kind: lore.RefKindPRNumber, Value: linkSlug + "#12"}),
			candidates: []entities.DocumentMeta{linkMeta(linkPRID, lore.DocTypePR)},
			want: entities.Edge{
				Src: linkCommitID, Dst: linkPRID,
				Kind: entities.EdgeKindCommitInPR, Confidence: 1.0,
			},
		},
		{
			name: "a pull request lists its commits",
			source: linkDoc(linkPRID, lore.DocTypePR, "three commits",
				lore.RawRef{Kind: lore.RefKindCommitSHA, Value: linkFullSHA}),
			candidates: []entities.DocumentMeta{linkMeta(linkCommitID, lore.DocTypeCommit)},
			want: entities.Edge{
				Src: linkPRID, Dst: linkCommitID,
				Kind: entities.EdgeKindCommitInPR, Confidence: 1.0,
			},
		},
		{
			name: "a pull request closes an issue",
			source: linkDoc(linkPRID, lore.DocTypePR, "closes the duplicate-hits report",
				lore.RawRef{Kind: lore.RefKindPRNumber, Value: linkSlug + "#9"}),
			candidates: []entities.DocumentMeta{linkMeta(linkIssueID, lore.DocTypeIssue)},
			want: entities.Edge{
				Src: linkPRID, Dst: linkIssueID,
				Kind: entities.EdgeKindPRClosesIssue, Confidence: 1.0,
			},
		},
		{
			name: "a url names an ingested page exactly",
			source: linkDoc(linkPRID, lore.DocTypePR, "design lives at "+linkPageURL,
				lore.RawRef{Kind: lore.RefKindURL, Value: linkPageURL}),
			candidates: []entities.DocumentMeta{linkMeta(linkPageID, lore.DocTypePage)},
			want: entities.Edge{
				Src: linkPRID, Dst: linkPageID,
				Kind: entities.EdgeKindReferencesDoc, Confidence: 1.0,
			},
		},
		{
			name: "an issue comment quotes an abbreviated sha",
			source: linkDoc(linkCommentID, lore.DocTypeIssueComment, "broke in abc1234",
				lore.RawRef{Kind: lore.RefKindCommitSHA, Value: "abc1234"}),
			candidates: []entities.DocumentMeta{linkMeta(linkCommitID, lore.DocTypeCommit)},
			want: entities.Edge{
				Src: linkCommentID, Dst: linkCommitID,
				Kind: entities.EdgeKindMentionsCommit, Confidence: 0.9,
			},
		},
		{
			name: "a ticket key names an ingested ticket",
			source: linkDoc(linkPRID, lore.DocTypePR, "tracked as "+linkTicket,
				lore.RawRef{Kind: lore.RefKindTicketKey, Value: linkTicket}),
			candidates: []entities.DocumentMeta{linkMeta(linkTicketID, lore.DocTypeTicket)},
			want: entities.Edge{
				Src: linkPRID, Dst: linkTicketID,
				Kind: entities.EdgeKindReferencesDoc, Confidence: 0.9,
			},
		},
		{
			name: "a supersede phrase shares the line with the reference",
			source: linkDoc(linkPRID, lore.DocTypePR, "Supersedes #4 for good.",
				lore.RawRef{Kind: lore.RefKindPRNumber, Value: linkSlug + "#4"}),
			candidates: []entities.DocumentMeta{linkMeta("github:pr:"+linkSlug+"/pull/4", lore.DocTypePR)},
			want: entities.Edge{
				Src: linkPRID, Dst: "github:pr:" + linkSlug + "/pull/4",
				Kind: entities.EdgeKindSupersedes, Confidence: 0.8,
			},
		},
		{
			name: "the supersede phrase sits on another line than the reference",
			source: linkDoc(linkPRID, lore.DocTypePR, "Supersedes the old plan.\nSee #4 for context.",
				lore.RawRef{Kind: lore.RefKindPRNumber, Value: linkSlug + "#4"}),
			candidates: []entities.DocumentMeta{linkMeta("github:pr:"+linkSlug+"/pull/4", lore.DocTypePR)},
			want: entities.Edge{
				Src: linkPRID, Dst: "github:pr:" + linkSlug + "/pull/4",
				Kind: entities.EdgeKindReferencesDoc, Confidence: 0.9,
			},
		},
		{
			name: "a longer identifier on the supersede line is a different reference",
			source: linkDoc(linkPRID, lore.DocTypePR, "Supersedes #42 for good.",
				lore.RawRef{Kind: lore.RefKindPRNumber, Value: linkSlug + "#4"}),
			candidates: []entities.DocumentMeta{linkMeta("github:pr:"+linkSlug+"/pull/4", lore.DocTypePR)},
			want: entities.Edge{
				Src: linkPRID, Dst: "github:pr:" + linkSlug + "/pull/4",
				Kind: entities.EdgeKindReferencesDoc, Confidence: 0.9,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newLinkMocks(t)
			// No UpsertPendingRefs is declared: a resolved ref must not also be retried.
			m.store.EXPECT().ResolveRef(gomock.Any(), tt.source.Refs[0].Value).Return(tt.candidates, nil)
			m.store.EXPECT().UpsertEdges(gomock.Any(), []entities.Edge{tt.want}).Return(nil)
			m.store.EXPECT().DeletePendingRefs(gomock.Any(), linkOnly(tt.source)).Return(nil)

			err := m.resolver().Link(context.Background(), []lore.Document{tt.source})
			if err != nil {
				t.Fatalf("Link() = %v, want nil", err)
			}
		})
	}
}

func TestLinkKeepsAReferenceItCannotPinToOneDocument(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		source     lore.Document
		candidates []entities.DocumentMeta
	}{
		{
			name: "nothing in the index matches",
			source: linkDoc(linkPRID, lore.DocTypePR, "design lives at "+linkPageURL,
				lore.RawRef{Kind: lore.RefKindURL, Value: linkPageURL}),
		},
		{
			name: "the only candidate is the referencing document itself",
			source: linkDoc(linkPRID, lore.DocTypePR, "see "+linkSlug+"#12",
				lore.RawRef{Kind: lore.RefKindPRNumber, Value: linkSlug + "#12"}),
			candidates: []entities.DocumentMeta{linkMeta(linkPRID, lore.DocTypePR)},
		},
		{
			name: "two commits share the abbreviated sha",
			source: linkDoc(linkCommentID, lore.DocTypeIssueComment, "broke in abc1234",
				lore.RawRef{Kind: lore.RefKindCommitSHA, Value: "abc1234"}),
			candidates: []entities.DocumentMeta{
				linkMeta(linkCommitID, lore.DocTypeCommit),
				linkMeta("github:commit:"+linkSlug+"/commit/abc1234ffffffffffffffffffffffffffffffff", lore.DocTypeCommit),
			},
		},
		{
			name: "the candidate is not a type the ref kind can name",
			source: linkDoc(linkCommentID, lore.DocTypeIssueComment, "broke in abc1234",
				lore.RawRef{Kind: lore.RefKindCommitSHA, Value: "abc1234"}),
			candidates: []entities.DocumentMeta{linkMeta(linkPageID, lore.DocTypePage)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newLinkMocks(t)
			// No UpsertEdges and no DeletePendingRefs are declared: an unpinned ref is not evidence.
			m.store.EXPECT().ResolveRef(gomock.Any(), tt.source.Refs[0].Value).Return(tt.candidates, nil)
			m.store.EXPECT().UpsertPendingRefs(gomock.Any(), linkOnly(tt.source)).Return(nil)

			err := m.resolver().Link(context.Background(), []lore.Document{tt.source})
			if err != nil {
				t.Fatalf("Link() = %v, want nil", err)
			}
		})
	}
}

func TestLinkTurnsAPathIntoAnEdgePerCommitThatTouchedIt(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	shas := []string{linkPathSHA(1), linkPathSHA(2), linkPathSHA(3)}
	source := linkPathDoc(linkPRID, lore.DocTypePR, "rewrites "+linkFile)

	m.expectLog(shas...)
	for _, sha := range shas {
		m.expectCommit(sha)
	}
	// A pull request pointed at a commit would be commit_in_pr, were the commit the body's subject.
	m.store.EXPECT().UpsertEdges(gomock.Any(), []entities.Edge{
		linkPathEdge(linkPRID, shas[0]),
		linkPathEdge(linkPRID, shas[1]),
		linkPathEdge(linkPRID, shas[2]),
	}).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []lore.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkTakesOnlyTheNewestCommitsOfALongHistory(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	shas := make([]string, linkPathCommitCap+2)
	for i := range shas {
		shas[i] = linkPathSHA(i)
	}
	source := linkPathDoc(linkPRID, lore.DocTypePR, "rewrites "+linkFile)

	m.expectLog(shas...)
	// The commits past the cap are given no expectation: resolving one fails the test.
	want := make([]entities.Edge, linkPathCommitCap)
	for i := range linkPathCommitCap {
		m.expectCommit(shas[i])
		want[i] = linkPathEdge(linkPRID, shas[i])
	}
	m.store.EXPECT().UpsertEdges(gomock.Any(), want).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []lore.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkLogsAPathOnceHoweverManyDocumentsNameIt(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	sha := linkPathSHA(1)
	pr := linkPathDoc(linkPRID, lore.DocTypePR, "rewrites "+linkFile)
	page := linkPathDoc(linkPageID, lore.DocTypePage, "we split "+linkFile+" in two")

	m.expectLog(sha)
	m.expectCommit(sha)
	m.store.EXPECT().UpsertEdges(gomock.Any(), []entities.Edge{
		linkPathEdge(linkPRID, sha),
		linkPathEdge(linkPageID, sha),
	}).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), []entities.PendingRef{
		{SourceDoc: linkPRID, Ref: pr.Refs[0]},
		{SourceDoc: linkPageID, Ref: page.Refs[0]},
	}).Return(nil)

	if err := m.resolver().Link(context.Background(), []lore.Document{pr, page}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkLeavesAPathNoCloneTracksPending(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	source := linkPathDoc(linkPRID, lore.DocTypePR, "rewrites "+linkFile)

	// No Log and no UpsertEdges are declared: an untracked path names no commit.
	m.git.EXPECT().HasFileAtHEAD(gomock.Any(), linkFile).Return(false, nil)
	m.store.EXPECT().UpsertPendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []lore.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkKeepsAPathPendingWhenGitFailsAndStillLinksTheRest(t *testing.T) {
	t.Parallel()

	pathRef := lore.RawRef{Kind: lore.RefKindFilePath, Value: linkFile}
	urlRef := lore.RawRef{Kind: lore.RefKindURL, Value: linkPageURL}
	source := linkDoc(linkPRID, lore.DocTypePR,
		"rewrites "+linkFile+", designed at "+linkPageURL, pathRef, urlRef)

	tests := map[string]func(m linkMocks){
		"the clone cannot be searched": func(m linkMocks) {
			m.git.EXPECT().HasFileAtHEAD(gomock.Any(), linkFile).Return(false, errLinkGit)
		},
		"the log cannot be read": func(m linkMocks) {
			m.git.EXPECT().HasFileAtHEAD(gomock.Any(), linkFile).Return(true, nil)
			m.git.EXPECT().Log(gomock.Any(), linkFile).Return(nil, errLinkGit)
		},
	}

	for name, fail := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := newLinkMocks(t)
			fail(m)
			m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).
				Return([]entities.DocumentMeta{linkMeta(linkPageID, lore.DocTypePage)}, nil)
			m.store.EXPECT().UpsertEdges(gomock.Any(), []entities.Edge{{
				Src: linkPRID, Dst: linkPageID,
				Kind: entities.EdgeKindReferencesDoc, Confidence: 1.0,
			}}).Return(nil)
			m.store.EXPECT().UpsertPendingRefs(gomock.Any(),
				[]entities.PendingRef{{SourceDoc: linkPRID, Ref: pathRef}}).Return(nil)
			m.store.EXPECT().DeletePendingRefs(gomock.Any(),
				[]entities.PendingRef{{SourceDoc: linkPRID, Ref: urlRef}}).Return(nil)

			if err := m.resolver().Link(context.Background(), []lore.Document{source}); err != nil {
				t.Fatalf("Link() = %v, want nil", err)
			}
		})
	}
}

func TestLinkSkipsALoggedCommitTheIndexNeverIngested(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	unsynced, ingested := linkPathSHA(1), linkPathSHA(2)
	source := linkPathDoc(linkPRID, lore.DocTypePR, "rewrites "+linkFile)

	m.expectLog(unsynced, ingested)
	m.store.EXPECT().ResolveRef(gomock.Any(), unsynced).Return(nil, nil)
	m.expectCommit(ingested)
	m.store.EXPECT().UpsertEdges(gomock.Any(),
		[]entities.Edge{linkPathEdge(linkPRID, ingested)}).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []lore.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkNeverReadsAPathAsASupersedingReference(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	sha := linkPathSHA(1)
	source := linkPathDoc(linkPRID, lore.DocTypePR, "Supersedes "+linkFile+" for good.")

	m.expectLog(sha)
	m.expectCommit(sha)
	m.store.EXPECT().UpsertEdges(gomock.Any(),
		[]entities.Edge{linkPathEdge(linkPRID, sha)}).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []lore.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkNeverPointsACommitAtItselfThroughItsOwnPath(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	own, earlier := linkPathSHA(1), linkPathSHA(2)
	source := linkPathDoc(linkCommitDocID(own), lore.DocTypeCommit, "rewrites "+linkFile)

	m.expectLog(own, earlier)
	m.expectCommit(own)
	m.expectCommit(earlier)
	m.store.EXPECT().UpsertEdges(gomock.Any(),
		[]entities.Edge{linkPathEdge(linkCommitDocID(own), earlier)}).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []lore.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkFailsTheRoundWhenALoggedCommitCannotBeResolved(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	sha := linkPathSHA(1)
	source := linkPathDoc(linkPRID, lore.DocTypePR, "rewrites "+linkFile)

	m.expectLog(sha)
	m.store.EXPECT().ResolveRef(gomock.Any(), sha).Return(nil, errLinkStore)

	err := m.resolver().Link(context.Background(), []lore.Document{source})
	if !internalerror.IsInternal(err) {
		t.Fatalf("Link() = %v (%s), want internal", err, internalerror.KindOf(err))
	}
	if !errors.Is(err, errLinkStore) {
		t.Errorf("Link() = %v, want the store's cause wrapped", err)
	}
}

func TestLinkPendingResolvesAReferenceOnceItsTargetIsIngested(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	source := linkDoc(linkPRID, lore.DocTypePR, "design lives at "+linkPageURL,
		lore.RawRef{Kind: lore.RefKindURL, Value: linkPageURL})
	pending := linkOnly(source)

	gomock.InOrder(
		m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Return(nil, nil),
		m.store.EXPECT().UpsertPendingRefs(gomock.Any(), pending).Return(nil),
		m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil),
		m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).
			Return([]entities.DocumentMeta{linkMeta(linkPageID, lore.DocTypePage)}, nil),
		m.store.EXPECT().DocumentsWithBody(gomock.Any(), []lore.DocID{linkPRID}).
			Return([]lore.Document{linkStored(source)}, nil),
		m.store.EXPECT().UpsertEdges(gomock.Any(), []entities.Edge{{
			Src: linkPRID, Dst: linkPageID,
			Kind: entities.EdgeKindReferencesDoc, Confidence: 1.0,
		}}).Return(nil),
		m.store.EXPECT().DeletePendingRefs(gomock.Any(), pending).Return(nil),
	)

	ctx := context.Background()
	r := m.resolver()
	if err := r.Link(ctx, []lore.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
	if err := r.LinkPending(ctx); err != nil {
		t.Fatalf("LinkPending() = %v, want nil", err)
	}
}

func TestLinkPendingRepeatsTheSameEdgeAndNeverReinstatesTheRef(t *testing.T) {
	t.Parallel()

	const rounds = 2

	m := newLinkMocks(t)
	source := linkDoc(linkPRID, lore.DocTypePR, "design lives at "+linkPageURL,
		lore.RawRef{Kind: lore.RefKindURL, Value: linkPageURL})
	pending := linkOnly(source)
	edge := entities.Edge{
		Src: linkPRID, Dst: linkPageID,
		Kind: entities.EdgeKindReferencesDoc, Confidence: 1.0,
	}

	// No UpsertPendingRefs is declared: a resolved ref must never be re-recorded.
	m.store.EXPECT().PendingRefs(gomock.Any()).Times(rounds).Return(pending, nil)
	m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Times(rounds).
		Return([]entities.DocumentMeta{linkMeta(linkPageID, lore.DocTypePage)}, nil)
	m.store.EXPECT().DocumentsWithBody(gomock.Any(), []lore.DocID{linkPRID}).Times(rounds).
		Return([]lore.Document{linkStored(source)}, nil)
	m.store.EXPECT().UpsertEdges(gomock.Any(), []entities.Edge{edge}).Times(rounds).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), pending).Times(rounds).Return(nil)

	r := m.resolver()
	for round := range rounds {
		if err := r.LinkPending(context.Background()); err != nil {
			t.Fatalf("LinkPending() round %d = %v, want nil", round+1, err)
		}
	}
}

func TestLinkPendingBatchesEveryWriteAndReadsOneSourceBodyOnce(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	source := linkDoc(linkPRID, lore.DocTypePR, "Replaces "+linkOldURL+".\nTracked as "+linkTicket+".",
		lore.RawRef{Kind: lore.RefKindURL, Value: linkOldURL},
		lore.RawRef{Kind: lore.RefKindTicketKey, Value: linkTicket},
		lore.RawRef{Kind: lore.RefKindCommitSHA, Value: linkFullSHA})

	pending := make([]entities.PendingRef, len(source.Refs))
	for i, ref := range source.Refs {
		pending[i] = entities.PendingRef{SourceDoc: source.ID, Ref: ref}
	}

	m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil)
	m.store.EXPECT().ResolveRef(gomock.Any(), linkOldURL).
		Return([]entities.DocumentMeta{linkMeta(linkOldPageID, lore.DocTypePage)}, nil)
	m.store.EXPECT().ResolveRef(gomock.Any(), linkTicket).
		Return([]entities.DocumentMeta{linkMeta(linkTicketID, lore.DocTypeTicket)}, nil)
	m.store.EXPECT().ResolveRef(gomock.Any(), linkFullSHA).
		Return([]entities.DocumentMeta{linkMeta(linkCommitID, lore.DocTypeCommit)}, nil)
	m.store.EXPECT().DocumentsWithBody(gomock.Any(), []lore.DocID{linkPRID}).Times(1).
		Return([]lore.Document{linkStored(source)}, nil)
	m.store.EXPECT().UpsertEdges(gomock.Any(), []entities.Edge{
		{Src: linkPRID, Dst: linkOldPageID, Kind: entities.EdgeKindSupersedes, Confidence: 0.8},
		{Src: linkPRID, Dst: linkTicketID, Kind: entities.EdgeKindReferencesDoc, Confidence: 0.9},
		{Src: linkPRID, Dst: linkCommitID, Kind: entities.EdgeKindCommitInPR, Confidence: 1.0},
	}).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), pending).Return(nil)

	if err := m.resolver().LinkPending(context.Background()); err != nil {
		t.Fatalf("LinkPending() = %v, want nil", err)
	}
}

func TestLinkPendingClassifiesStoreFailures(t *testing.T) {
	t.Parallel()

	source := linkDoc(linkPRID, lore.DocTypePR, "design lives at "+linkPageURL,
		lore.RawRef{Kind: lore.RefKindURL, Value: linkPageURL})
	pending := linkOnly(source)
	page := []entities.DocumentMeta{linkMeta(linkPageID, lore.DocTypePage)}

	tests := map[string]func(m linkMocks){
		"the pending refs cannot be read": func(m linkMocks) {
			m.store.EXPECT().PendingRefs(gomock.Any()).Return(nil, errLinkStore)
		},
		"a ref cannot be resolved": func(m linkMocks) {
			m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil)
			m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Return(nil, errLinkStore)
		},
		"the referencing body cannot be read": func(m linkMocks) {
			m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil)
			m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Return(page, nil)
			m.store.EXPECT().DocumentsWithBody(gomock.Any(), []lore.DocID{linkPRID}).
				Return(nil, errLinkStore)
		},
		"the edges cannot be stored": func(m linkMocks) {
			m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil)
			m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Return(page, nil)
			m.store.EXPECT().DocumentsWithBody(gomock.Any(), gomock.Any()).
				Return([]lore.Document{linkStored(source)}, nil)
			m.store.EXPECT().UpsertEdges(gomock.Any(), gomock.Any()).Return(errLinkStore)
		},
		"the resolved refs cannot be cleared": func(m linkMocks) {
			m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil)
			m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Return(page, nil)
			m.store.EXPECT().DocumentsWithBody(gomock.Any(), gomock.Any()).
				Return([]lore.Document{linkStored(source)}, nil)
			m.store.EXPECT().UpsertEdges(gomock.Any(), gomock.Any()).Return(nil)
			m.store.EXPECT().DeletePendingRefs(gomock.Any(), pending).Return(errLinkStore)
		},
		"an unresolved ref cannot be recorded": func(m linkMocks) {
			m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil)
			m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Return(nil, nil)
			m.store.EXPECT().UpsertPendingRefs(gomock.Any(), pending).Return(errLinkStore)
		},
	}

	for name, expect := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := newLinkMocks(t)
			expect(m)

			err := m.resolver().LinkPending(context.Background())
			if !internalerror.IsInternal(err) {
				t.Fatalf("LinkPending() = %v (%s), want internal", err, internalerror.KindOf(err))
			}
			if !errors.Is(err, errLinkStore) {
				t.Errorf("LinkPending() = %v, want the store's cause wrapped", err)
			}
		})
	}
}

func TestLinkWithoutRefsTouchesNothing(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)

	docs := []lore.Document{linkDoc(linkPRID, lore.DocTypePR, "no references at all")}
	if err := m.resolver().Link(context.Background(), docs); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}
