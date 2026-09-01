package services_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"

	"lore/internal/connectors/gitrepo"
	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	mock_gitrepo "lore/internal/mocks/gitrepo"
	mock_repositories "lore/internal/mocks/repositories"
	"lore/internal/services"
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
	linkCommitID  = entities.NewDocID("github", entities.DocTypeCommit, linkSlug+"/commit/"+linkFullSHA)
	linkPRID      = entities.NewDocID("github", entities.DocTypePR, linkSlug+"/pull/12")
	linkIssueID   = entities.NewDocID("github", entities.DocTypeIssue, linkSlug+"/issues/9")
	linkCommentID = entities.NewDocID("github", entities.DocTypeIssueComment, linkSlug+"/issues/9#c1")
	linkPageID    = entities.NewDocID("notion", entities.DocTypePage, "design/retrieval")
	linkOldPageID = entities.NewDocID("notion", entities.DocTypePage, "design/retrieval-v1")
	linkTicketID  = entities.NewDocID("jira", entities.DocTypeTicket, linkTicket)
)

var (
	errLinkStore = errors.New("store is on fire")
	errLinkGit   = errors.New("clone is unreadable")
)

type linkMocks struct {
	store *mock_repositories.MockIndexStore
	git   *mock_gitrepo.MockGitRepo
}

func newLinkMocks(t *testing.T) linkMocks {
	t.Helper()

	ctrl := gomock.NewController(t)

	return linkMocks{
		store: mock_repositories.NewMockIndexStore(ctrl),
		git:   mock_gitrepo.NewMockGitRepo(ctrl),
	}
}

func (m linkMocks) resolver() services.LinkResolver {
	return services.NewLinkResolver(m.store, []services.CodeRepo{{Path: linkClone, Git: m.git}})
}

func (m linkMocks) expectLog(shas ...string) {
	m.git.EXPECT().HasFileAtHEAD(gomock.Any(), linkFile).Return(true, nil)

	log := make([]gitrepo.CommitRef, len(shas))
	for i, sha := range shas {
		log[i] = gitrepo.CommitRef{SHA: sha}
	}
	m.git.EXPECT().Log(gomock.Any(), linkFile).Return(log, nil)
}

func (m linkMocks) expectCommit(sha string) *gomock.Call {
	return m.store.EXPECT().ResolveRef(gomock.Any(), sha).
		Return([]entities.DocumentMeta{linkMeta(linkCommitDocID(sha), entities.DocTypeCommit)}, nil)
}

func linkPathSHA(n int) string { return fmt.Sprintf("%040x", n) }

func linkCommitDocID(sha string) entities.DocID {
	return entities.NewDocID("github", entities.DocTypeCommit, linkSlug+"/commit/"+sha)
}

func linkPathDoc(id entities.DocID, docType entities.DocType, body string) entities.Document {
	return linkDoc(id, docType, body, entities.RawRef{Kind: entities.RefKindFilePath, Value: linkFile})
}

func linkPathEdge(source entities.DocID, sha string) entities.Edge {
	return entities.Edge{
		Src: source, Dst: linkCommitDocID(sha),
		Kind: entities.EdgeKindMentionsPath, Confidence: 0.7,
	}
}

func linkDoc(id entities.DocID, docType entities.DocType, body string, refs ...entities.RawRef) entities.Document {
	return entities.Document{
		ID:     id,
		Source: "github",
		Type:   docType,
		Title:  "Resolve refs after the batch commits",
		Body:   body,
		Refs:   refs,
	}
}

func linkMeta(id entities.DocID, docType entities.DocType) entities.DocumentMeta {
	return entities.DocumentMeta{ID: id, Type: docType}
}

func linkStored(doc entities.Document) entities.Document {
	doc.Refs = nil

	return doc
}

func linkOnly(doc entities.Document) []entities.PendingRef {
	return []entities.PendingRef{{SourceDoc: doc.ID, Ref: doc.Refs[0]}}
}

func TestLinkWritesTheEdgeEachRuleDictates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		source     entities.Document
		candidates []entities.DocumentMeta
		want       entities.Edge
	}{
		{
			name: "a commit names the pull request that carried it",
			source: linkDoc(linkCommitID, entities.DocTypeCommit, "fix the resolver",
				entities.RawRef{Kind: entities.RefKindPRNumber, Value: linkSlug + "#12"}),
			candidates: []entities.DocumentMeta{linkMeta(linkPRID, entities.DocTypePR)},
			want: entities.Edge{
				Src: linkCommitID, Dst: linkPRID,
				Kind: entities.EdgeKindCommitInPR, Confidence: 1.0,
			},
		},
		{
			name: "a pull request lists its commits",
			source: linkDoc(linkPRID, entities.DocTypePR, "three commits",
				entities.RawRef{Kind: entities.RefKindCommitSHA, Value: linkFullSHA}),
			candidates: []entities.DocumentMeta{linkMeta(linkCommitID, entities.DocTypeCommit)},
			want: entities.Edge{
				Src: linkPRID, Dst: linkCommitID,
				Kind: entities.EdgeKindCommitInPR, Confidence: 1.0,
			},
		},
		{
			name: "a pull request closes an issue",
			source: linkDoc(linkPRID, entities.DocTypePR, "closes the duplicate-hits report",
				entities.RawRef{Kind: entities.RefKindPRNumber, Value: linkSlug + "#9"}),
			candidates: []entities.DocumentMeta{linkMeta(linkIssueID, entities.DocTypeIssue)},
			want: entities.Edge{
				Src: linkPRID, Dst: linkIssueID,
				Kind: entities.EdgeKindPRClosesIssue, Confidence: 1.0,
			},
		},
		{
			name: "a url names an ingested page exactly",
			source: linkDoc(linkPRID, entities.DocTypePR, "design lives at "+linkPageURL,
				entities.RawRef{Kind: entities.RefKindURL, Value: linkPageURL}),
			candidates: []entities.DocumentMeta{linkMeta(linkPageID, entities.DocTypePage)},
			want: entities.Edge{
				Src: linkPRID, Dst: linkPageID,
				Kind: entities.EdgeKindReferencesDoc, Confidence: 1.0,
			},
		},
		{
			name: "an issue comment quotes an abbreviated sha",
			source: linkDoc(linkCommentID, entities.DocTypeIssueComment, "broke in abc1234",
				entities.RawRef{Kind: entities.RefKindCommitSHA, Value: "abc1234"}),
			candidates: []entities.DocumentMeta{linkMeta(linkCommitID, entities.DocTypeCommit)},
			want: entities.Edge{
				Src: linkCommentID, Dst: linkCommitID,
				Kind: entities.EdgeKindMentionsCommit, Confidence: 0.9,
			},
		},
		{
			name: "a ticket key names an ingested ticket",
			source: linkDoc(linkPRID, entities.DocTypePR, "tracked as "+linkTicket,
				entities.RawRef{Kind: entities.RefKindTicketKey, Value: linkTicket}),
			candidates: []entities.DocumentMeta{linkMeta(linkTicketID, entities.DocTypeTicket)},
			want: entities.Edge{
				Src: linkPRID, Dst: linkTicketID,
				Kind: entities.EdgeKindReferencesDoc, Confidence: 0.9,
			},
		},
		{
			name: "a supersede phrase shares the line with the reference",
			source: linkDoc(linkPRID, entities.DocTypePR, "Supersedes #4 for good.",
				entities.RawRef{Kind: entities.RefKindPRNumber, Value: linkSlug + "#4"}),
			candidates: []entities.DocumentMeta{linkMeta("github:pr:"+linkSlug+"/pull/4", entities.DocTypePR)},
			want: entities.Edge{
				Src: linkPRID, Dst: "github:pr:" + linkSlug + "/pull/4",
				Kind: entities.EdgeKindSupersedes, Confidence: 0.8,
			},
		},
		{
			name: "the supersede phrase sits on another line than the reference",
			source: linkDoc(linkPRID, entities.DocTypePR, "Supersedes the old plan.\nSee #4 for context.",
				entities.RawRef{Kind: entities.RefKindPRNumber, Value: linkSlug + "#4"}),
			candidates: []entities.DocumentMeta{linkMeta("github:pr:"+linkSlug+"/pull/4", entities.DocTypePR)},
			want: entities.Edge{
				Src: linkPRID, Dst: "github:pr:" + linkSlug + "/pull/4",
				Kind: entities.EdgeKindReferencesDoc, Confidence: 0.9,
			},
		},
		{
			name: "a longer identifier on the supersede line is a different reference",
			source: linkDoc(linkPRID, entities.DocTypePR, "Supersedes #42 for good.",
				entities.RawRef{Kind: entities.RefKindPRNumber, Value: linkSlug + "#4"}),
			candidates: []entities.DocumentMeta{linkMeta("github:pr:"+linkSlug+"/pull/4", entities.DocTypePR)},
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

			err := m.resolver().Link(context.Background(), []entities.Document{tt.source})
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
		source     entities.Document
		candidates []entities.DocumentMeta
	}{
		{
			name: "nothing in the index matches",
			source: linkDoc(linkPRID, entities.DocTypePR, "design lives at "+linkPageURL,
				entities.RawRef{Kind: entities.RefKindURL, Value: linkPageURL}),
		},
		{
			name: "the only candidate is the referencing document itself",
			source: linkDoc(linkPRID, entities.DocTypePR, "see "+linkSlug+"#12",
				entities.RawRef{Kind: entities.RefKindPRNumber, Value: linkSlug + "#12"}),
			candidates: []entities.DocumentMeta{linkMeta(linkPRID, entities.DocTypePR)},
		},
		{
			name: "two commits share the abbreviated sha",
			source: linkDoc(linkCommentID, entities.DocTypeIssueComment, "broke in abc1234",
				entities.RawRef{Kind: entities.RefKindCommitSHA, Value: "abc1234"}),
			candidates: []entities.DocumentMeta{
				linkMeta(linkCommitID, entities.DocTypeCommit),
				linkMeta("github:commit:"+linkSlug+"/commit/abc1234ffffffffffffffffffffffffffffffff", entities.DocTypeCommit),
			},
		},
		{
			name: "the candidate is not a type the ref kind can name",
			source: linkDoc(linkCommentID, entities.DocTypeIssueComment, "broke in abc1234",
				entities.RawRef{Kind: entities.RefKindCommitSHA, Value: "abc1234"}),
			candidates: []entities.DocumentMeta{linkMeta(linkPageID, entities.DocTypePage)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newLinkMocks(t)
			// No UpsertEdges and no DeletePendingRefs are declared: an unpinned ref is not evidence.
			m.store.EXPECT().ResolveRef(gomock.Any(), tt.source.Refs[0].Value).Return(tt.candidates, nil)
			m.store.EXPECT().UpsertPendingRefs(gomock.Any(), linkOnly(tt.source)).Return(nil)

			err := m.resolver().Link(context.Background(), []entities.Document{tt.source})
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
	source := linkPathDoc(linkPRID, entities.DocTypePR, "rewrites "+linkFile)

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

	if err := m.resolver().Link(context.Background(), []entities.Document{source}); err != nil {
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
	source := linkPathDoc(linkPRID, entities.DocTypePR, "rewrites "+linkFile)

	m.expectLog(shas...)
	// The commits past the cap are given no expectation: resolving one fails the test.
	want := make([]entities.Edge, linkPathCommitCap)
	for i := range linkPathCommitCap {
		m.expectCommit(shas[i])
		want[i] = linkPathEdge(linkPRID, shas[i])
	}
	m.store.EXPECT().UpsertEdges(gomock.Any(), want).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []entities.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkLogsAPathOnceHoweverManyDocumentsNameIt(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	sha := linkPathSHA(1)
	pr := linkPathDoc(linkPRID, entities.DocTypePR, "rewrites "+linkFile)
	page := linkPathDoc(linkPageID, entities.DocTypePage, "we split "+linkFile+" in two")

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

	if err := m.resolver().Link(context.Background(), []entities.Document{pr, page}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkLeavesAPathNoCloneTracksPending(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	source := linkPathDoc(linkPRID, entities.DocTypePR, "rewrites "+linkFile)

	// No Log and no UpsertEdges are declared: an untracked path names no commit.
	m.git.EXPECT().HasFileAtHEAD(gomock.Any(), linkFile).Return(false, nil)
	m.store.EXPECT().UpsertPendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []entities.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkKeepsAPathPendingWhenGitFailsAndStillLinksTheRest(t *testing.T) {
	t.Parallel()

	pathRef := entities.RawRef{Kind: entities.RefKindFilePath, Value: linkFile}
	urlRef := entities.RawRef{Kind: entities.RefKindURL, Value: linkPageURL}
	source := linkDoc(linkPRID, entities.DocTypePR,
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
				Return([]entities.DocumentMeta{linkMeta(linkPageID, entities.DocTypePage)}, nil)
			m.store.EXPECT().UpsertEdges(gomock.Any(), []entities.Edge{{
				Src: linkPRID, Dst: linkPageID,
				Kind: entities.EdgeKindReferencesDoc, Confidence: 1.0,
			}}).Return(nil)
			m.store.EXPECT().UpsertPendingRefs(gomock.Any(),
				[]entities.PendingRef{{SourceDoc: linkPRID, Ref: pathRef}}).Return(nil)
			m.store.EXPECT().DeletePendingRefs(gomock.Any(),
				[]entities.PendingRef{{SourceDoc: linkPRID, Ref: urlRef}}).Return(nil)

			if err := m.resolver().Link(context.Background(), []entities.Document{source}); err != nil {
				t.Fatalf("Link() = %v, want nil", err)
			}
		})
	}
}

func TestLinkSkipsALoggedCommitTheIndexNeverIngested(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	unsynced, ingested := linkPathSHA(1), linkPathSHA(2)
	source := linkPathDoc(linkPRID, entities.DocTypePR, "rewrites "+linkFile)

	m.expectLog(unsynced, ingested)
	m.store.EXPECT().ResolveRef(gomock.Any(), unsynced).Return(nil, nil)
	m.expectCommit(ingested)
	m.store.EXPECT().UpsertEdges(gomock.Any(),
		[]entities.Edge{linkPathEdge(linkPRID, ingested)}).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []entities.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkNeverReadsAPathAsASupersedingReference(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	sha := linkPathSHA(1)
	source := linkPathDoc(linkPRID, entities.DocTypePR, "Supersedes "+linkFile+" for good.")

	m.expectLog(sha)
	m.expectCommit(sha)
	m.store.EXPECT().UpsertEdges(gomock.Any(),
		[]entities.Edge{linkPathEdge(linkPRID, sha)}).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []entities.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkNeverPointsACommitAtItselfThroughItsOwnPath(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	own, earlier := linkPathSHA(1), linkPathSHA(2)
	source := linkPathDoc(linkCommitDocID(own), entities.DocTypeCommit, "rewrites "+linkFile)

	m.expectLog(own, earlier)
	m.expectCommit(own)
	m.expectCommit(earlier)
	m.store.EXPECT().UpsertEdges(gomock.Any(),
		[]entities.Edge{linkPathEdge(linkCommitDocID(own), earlier)}).Return(nil)
	m.store.EXPECT().DeletePendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []entities.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}

func TestLinkFailsTheRoundWhenALoggedCommitCannotBeResolved(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	sha := linkPathSHA(1)
	source := linkPathDoc(linkPRID, entities.DocTypePR, "rewrites "+linkFile)

	m.expectLog(sha)
	m.store.EXPECT().ResolveRef(gomock.Any(), sha).Return(nil, errLinkStore)

	err := m.resolver().Link(context.Background(), []entities.Document{source})
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
	source := linkDoc(linkPRID, entities.DocTypePR, "design lives at "+linkPageURL,
		entities.RawRef{Kind: entities.RefKindURL, Value: linkPageURL})
	pending := linkOnly(source)

	gomock.InOrder(
		m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Return(nil, nil),
		m.store.EXPECT().UpsertPendingRefs(gomock.Any(), pending).Return(nil),
		m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil),
		m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).
			Return([]entities.DocumentMeta{linkMeta(linkPageID, entities.DocTypePage)}, nil),
		m.store.EXPECT().DocumentsWithBody(gomock.Any(), []entities.DocID{linkPRID}).
			Return([]entities.Document{linkStored(source)}, nil),
		m.store.EXPECT().UpsertEdges(gomock.Any(), []entities.Edge{{
			Src: linkPRID, Dst: linkPageID,
			Kind: entities.EdgeKindReferencesDoc, Confidence: 1.0,
		}}).Return(nil),
		m.store.EXPECT().DeletePendingRefs(gomock.Any(), pending).Return(nil),
	)

	ctx := context.Background()
	r := m.resolver()
	if err := r.Link(ctx, []entities.Document{source}); err != nil {
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
	source := linkDoc(linkPRID, entities.DocTypePR, "design lives at "+linkPageURL,
		entities.RawRef{Kind: entities.RefKindURL, Value: linkPageURL})
	pending := linkOnly(source)
	edge := entities.Edge{
		Src: linkPRID, Dst: linkPageID,
		Kind: entities.EdgeKindReferencesDoc, Confidence: 1.0,
	}

	// No UpsertPendingRefs is declared: a resolved ref must never be re-recorded.
	m.store.EXPECT().PendingRefs(gomock.Any()).Times(rounds).Return(pending, nil)
	m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Times(rounds).
		Return([]entities.DocumentMeta{linkMeta(linkPageID, entities.DocTypePage)}, nil)
	m.store.EXPECT().DocumentsWithBody(gomock.Any(), []entities.DocID{linkPRID}).Times(rounds).
		Return([]entities.Document{linkStored(source)}, nil)
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
	source := linkDoc(linkPRID, entities.DocTypePR, "Replaces "+linkOldURL+".\nTracked as "+linkTicket+".",
		entities.RawRef{Kind: entities.RefKindURL, Value: linkOldURL},
		entities.RawRef{Kind: entities.RefKindTicketKey, Value: linkTicket},
		entities.RawRef{Kind: entities.RefKindCommitSHA, Value: linkFullSHA})

	pending := make([]entities.PendingRef, len(source.Refs))
	for i, ref := range source.Refs {
		pending[i] = entities.PendingRef{SourceDoc: source.ID, Ref: ref}
	}

	m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil)
	m.store.EXPECT().ResolveRef(gomock.Any(), linkOldURL).
		Return([]entities.DocumentMeta{linkMeta(linkOldPageID, entities.DocTypePage)}, nil)
	m.store.EXPECT().ResolveRef(gomock.Any(), linkTicket).
		Return([]entities.DocumentMeta{linkMeta(linkTicketID, entities.DocTypeTicket)}, nil)
	m.store.EXPECT().ResolveRef(gomock.Any(), linkFullSHA).
		Return([]entities.DocumentMeta{linkMeta(linkCommitID, entities.DocTypeCommit)}, nil)
	m.store.EXPECT().DocumentsWithBody(gomock.Any(), []entities.DocID{linkPRID}).Times(1).
		Return([]entities.Document{linkStored(source)}, nil)
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

	source := linkDoc(linkPRID, entities.DocTypePR, "design lives at "+linkPageURL,
		entities.RawRef{Kind: entities.RefKindURL, Value: linkPageURL})
	pending := linkOnly(source)
	page := []entities.DocumentMeta{linkMeta(linkPageID, entities.DocTypePage)}

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
			m.store.EXPECT().DocumentsWithBody(gomock.Any(), []entities.DocID{linkPRID}).
				Return(nil, errLinkStore)
		},
		"the edges cannot be stored": func(m linkMocks) {
			m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil)
			m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Return(page, nil)
			m.store.EXPECT().DocumentsWithBody(gomock.Any(), gomock.Any()).
				Return([]entities.Document{linkStored(source)}, nil)
			m.store.EXPECT().UpsertEdges(gomock.Any(), gomock.Any()).Return(errLinkStore)
		},
		"the resolved refs cannot be cleared": func(m linkMocks) {
			m.store.EXPECT().PendingRefs(gomock.Any()).Return(pending, nil)
			m.store.EXPECT().ResolveRef(gomock.Any(), linkPageURL).Return(page, nil)
			m.store.EXPECT().DocumentsWithBody(gomock.Any(), gomock.Any()).
				Return([]entities.Document{linkStored(source)}, nil)
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

	docs := []entities.Document{linkDoc(linkPRID, entities.DocTypePR, "no references at all")}
	if err := m.resolver().Link(context.Background(), docs); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
	}
}
