package services

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/repositories/sqlite"
	"github.com/setthasit/Lore/sdk"
)

const (
	xrefDims = 3
	xrefSlug = "acme/lore"
	xrefSHA  = "9f1a2b3c4d5e6f708192a3b4c5d6e7f80912a3b4"

	xrefTicketKey  = "PROJ-123"
	xrefMissingKey = "PROJ-777"
	xrefLateKey    = "PROJ-42"
	xrefLateURL    = "https://acme.atlassian.net/browse/" + xrefLateKey
)

var (
	xrefCommitID = lore.NewDocID("github", lore.DocTypeCommit, xrefSlug+"/commit/"+xrefSHA)
	xrefTicketID = lore.NewDocID("jira", lore.DocTypeTicket, xrefTicketKey)
	xrefPageID   = lore.NewDocID("notion", lore.DocTypePage, "design/auth-rollout")
	xrefLateID   = lore.NewDocID("jira", lore.DocTypeTicket, xrefLateKey)
)

func xrefStore(t *testing.T) *sqlite.Store {
	t.Helper()

	s, err := sqlite.Open(filepath.Join(t.TempDir(), "workspace.db"), xrefDims)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s
}

func xrefIngest(t *testing.T, s *sqlite.Store, docs ...lore.Document) {
	t.Helper()

	if err := s.UpsertDocuments(context.Background(), docs); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
}

func xrefCommit(body string, refs ...lore.RawRef) lore.Document {
	return lore.Document{
		ID:      xrefCommitID,
		Source:  "github",
		Type:    lore.DocTypeCommit,
		RepoRef: "github:" + xrefSlug,
		Title:   "Send tenants to their own landing page",
		Body:    body,
		Author:  "dana",
		URL:     "https://github.com/" + xrefSlug + "/commit/" + xrefSHA,
		Refs:    refs,
	}
}

func xrefTicket() lore.Document {
	return lore.Document{
		ID:     xrefTicketID,
		Source: "jira",
		Type:   lore.DocTypeTicket,
		Title:  "Post-login redirect drops the tenant",
		Body:   "Signing in lands the user on the wrong tenant.",
		Author: "sam",
		URL:    "https://acme.atlassian.net/browse/" + xrefTicketKey,
	}
}

func xrefEdges(t *testing.T, s *sqlite.Store, ids ...lore.DocID) []entities.Edge {
	t.Helper()

	edges, err := s.Neighbors(context.Background(), ids, nil, entities.DirBoth)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}

	return edges
}

func xrefPending(t *testing.T, s *sqlite.Store) []entities.PendingRef {
	t.Helper()

	refs, err := s.PendingRefs(context.Background())
	if err != nil {
		t.Fatalf("PendingRefs: %v", err)
	}

	return refs
}

func xrefAssertEdges(t *testing.T, what string, got, want []entities.Edge) {
	t.Helper()

	slices.SortFunc(got, walkEdgeOrder)
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func xrefAssertPending(t *testing.T, what string, got, want []entities.PendingRef) {
	t.Helper()

	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestLinkResolverPointsAGitHubCommitAtItsJiraTicket(t *testing.T) {
	ctx := context.Background()
	store := xrefStore(t)

	commit := xrefCommit("Fix the post-login redirect described in "+xrefTicketKey+".",
		lore.RawRef{Kind: lore.RefKindTicketKey, Value: xrefTicketKey})
	ticket := xrefTicket()
	xrefIngest(t, store, commit, ticket)

	if err := NewLinkResolver(store, nil).Link(ctx, []lore.Document{commit, ticket}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	want := []entities.Edge{{
		Src:        xrefCommitID,
		Dst:        xrefTicketID,
		Kind:       entities.EdgeKindReferencesDoc,
		Confidence: 0.9,
	}}
	xrefAssertEdges(t, "corpus edges", xrefEdges(t, store, xrefCommitID, xrefTicketID), want)
	xrefAssertPending(t, "pending refs", xrefPending(t, store), nil)

	inbound, err := store.Neighbors(ctx, []lore.DocID{xrefTicketID}, nil, entities.DirIn)
	if err != nil {
		t.Fatalf("Neighbors in: %v", err)
	}
	xrefAssertEdges(t, "edges into the ticket", inbound, want)

	outbound, err := store.Neighbors(ctx, []lore.DocID{xrefTicketID}, nil, entities.DirOut)
	if err != nil {
		t.Fatalf("Neighbors out: %v", err)
	}
	xrefAssertEdges(t, "edges out of the ticket", outbound, nil)
}

func TestLinkResolverLeavesAnUnmatchedTicketKeyPending(t *testing.T) {
	ctx := context.Background()
	store := xrefStore(t)

	ref := lore.RawRef{Kind: lore.RefKindTicketKey, Value: xrefMissingKey}
	commit := xrefCommit("Groundwork for "+xrefMissingKey+", which nothing has filed yet.", ref)
	ticket := xrefTicket()
	xrefIngest(t, store, commit, ticket)

	if err := NewLinkResolver(store, nil).Link(ctx, []lore.Document{commit, ticket}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	xrefAssertEdges(t, "corpus edges", xrefEdges(t, store, xrefCommitID, xrefTicketID), nil)
	xrefAssertPending(t, "pending refs", xrefPending(t, store),
		[]entities.PendingRef{{SourceDoc: xrefCommitID, Ref: ref}})
}

func TestLinkResolverResolvesADeferredRefOnALaterSyncRound(t *testing.T) {
	ctx := context.Background()
	store := xrefStore(t)
	resolver := NewLinkResolver(store, nil)

	keyRef := lore.RawRef{Kind: lore.RefKindTicketKey, Value: xrefLateKey}
	urlRef := lore.RawRef{Kind: lore.RefKindURL, Value: xrefLateURL}
	page := lore.Document{
		ID:     xrefPageID,
		Source: "notion",
		Type:   lore.DocTypePage,
		Title:  "Auth rollout decision",
		Body: "We chose the staged rollout tracked by " + xrefLateKey +
			", see " + xrefLateURL + " for the acceptance criteria.",
		Author: "dana",
		URL:    "https://notion.so/design/auth-rollout",
		Refs:   []lore.RawRef{keyRef, urlRef},
	}

	xrefIngest(t, store, page)
	if err := resolver.Link(ctx, []lore.Document{page}); err != nil {
		t.Fatalf("round 1 Link: %v", err)
	}

	corpus := []lore.DocID{xrefPageID, xrefLateID}
	deferred := []entities.PendingRef{
		{SourceDoc: xrefPageID, Ref: keyRef},
		{SourceDoc: xrefPageID, Ref: urlRef},
	}
	xrefAssertEdges(t, "round 1 edges", xrefEdges(t, store, corpus...), nil)
	xrefAssertPending(t, "round 1 pending refs", xrefPending(t, store), deferred)

	xrefIngest(t, store, lore.Document{
		ID:     xrefLateID,
		Source: "jira",
		Type:   lore.DocTypeTicket,
		Title:  "Stage the auth rollout behind a flag",
		Body:   "Enable the new provider one tenant at a time.",
		Author: "sam",
		URL:    xrefLateURL,
	})
	if err := resolver.LinkPending(ctx); err != nil {
		t.Fatalf("round 2 LinkPending: %v", err)
	}

	// The url ref's exact match outranks the ticket-key guess for the same edge.
	round2 := xrefEdges(t, store, corpus...)
	xrefAssertEdges(t, "round 2 edges", round2, []entities.Edge{{
		Src:        xrefPageID,
		Dst:        xrefLateID,
		Kind:       entities.EdgeKindReferencesDoc,
		Confidence: 1.0,
	}})
	xrefAssertPending(t, "round 2 pending refs", xrefPending(t, store), nil)

	if err := resolver.LinkPending(ctx); err != nil {
		t.Fatalf("round 3 LinkPending: %v", err)
	}
	xrefAssertEdges(t, "round 3 edges", xrefEdges(t, store, corpus...), round2)
	xrefAssertPending(t, "round 3 pending refs", xrefPending(t, store), nil)
}
