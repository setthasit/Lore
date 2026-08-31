package services_test

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	mock_repositories "lore/internal/mocks/repositories"
	"lore/internal/services"
)

const (
	linkSlug    = "acme/lore"
	linkFullSHA = "9f1a2b3c4d5e6f708192a3b4c5d6e7f80912a3b4"
	linkPageURL = "https://notion.so/design/retrieval"
	linkOldURL  = "https://notion.so/design/retrieval-v1"
	linkTicket  = "PROJ-123"
)

var (
	linkCommitID  = entities.NewDocID("github", entities.DocTypeCommit, linkSlug+"/commit/"+linkFullSHA)
	linkPRID      = entities.NewDocID("github", entities.DocTypePR, linkSlug+"/pull/12")
	linkIssueID   = entities.NewDocID("github", entities.DocTypeIssue, linkSlug+"/issues/9")
	linkCommentID = entities.NewDocID("github", entities.DocTypeIssueComment, linkSlug+"/issues/9#c1")
	linkPageID    = entities.NewDocID("notion", entities.DocTypePage, "design/retrieval")
	linkOldPageID = entities.NewDocID("notion", entities.DocTypePage, "design/retrieval-v1")
	linkTicketID  = entities.NewDocID("jira", entities.DocTypeTicket, linkTicket)
)

var errLinkStore = errors.New("store is on fire")

type linkMocks struct {
	store *mock_repositories.MockIndexStore
}

func newLinkMocks(t *testing.T) linkMocks {
	t.Helper()

	return linkMocks{store: mock_repositories.NewMockIndexStore(gomock.NewController(t))}
}

func (m linkMocks) resolver() services.LinkResolver {
	return services.NewLinkResolver(m.store)
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

func TestLinkNeverAsksTheStoreAboutAFilePath(t *testing.T) {
	t.Parallel()

	m := newLinkMocks(t)
	source := linkDoc(linkPRID, entities.DocTypePR, "rewrites internal/auth/auth.go",
		entities.RawRef{Kind: entities.RefKindFilePath, Value: "internal/auth/auth.go"})

	// No ResolveRef and no UpsertEdges are declared: paths are not documents yet.
	m.store.EXPECT().UpsertPendingRefs(gomock.Any(), linkOnly(source)).Return(nil)

	if err := m.resolver().Link(context.Background(), []entities.Document{source}); err != nil {
		t.Fatalf("Link() = %v, want nil", err)
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
