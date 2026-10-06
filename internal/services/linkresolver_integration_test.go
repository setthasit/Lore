package services

import (
	"context"
	"fmt"
	"iter"
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

	xrefJiraEU    = "jira-eu"
	xrefJiraUS    = "jira-us"
	xrefSourceKey = "PROJ-9"
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

func xrefInstanceTicket(instance, key, body string, refs ...lore.RawRef) lore.Document {
	return lore.Document{
		ID:     lore.NewDocID(instance, lore.DocTypeTicket, key),
		Source: instance,
		Type:   lore.DocTypeTicket,
		Title:  "Post-login redirect drops the tenant",
		Body:   body,
		Author: "sam",
		URL:    "https://" + instance + ".atlassian.net/browse/" + key,
		Refs:   refs,
	}
}

func xrefNotionPages() (lore.Document, lore.Document) {
	ref := lore.RawRef{
		Kind:  lore.RefKindURL,
		Value: "https://www.notion.so/3e7a409f814e80a98644fc0217b75f69",
	}
	source := lore.Document{
		ID:     lore.NewDocID("notion", lore.DocTypePage, "11111111-2222-3333-4444-555555555555"),
		Source: "notion",
		Type:   lore.DocTypePage,
		Title:  "White paper review",
		Body:   "Read the white paper at " + ref.Value,
		URL:    "https://app.notion.com/p/White-Paper-Review-11111111222233334444555555555555",
		Refs:   []lore.RawRef{ref},
	}
	target := lore.Document{
		ID:     lore.NewDocID("notion", lore.DocTypePage, "3e7a409f-814e-80a9-8644-fc0217b75f69"),
		Source: "notion",
		Type:   lore.DocTypePage,
		Title:  "Still Got It White Paper",
		Body:   "White paper findings.",
		URL:    "https://app.notion.com/p/Still-Got-It-White-Paper-3e7a409f814e80a98644fc0217b75f69",
	}

	return source, target
}

func xrefNotionRelationPages() (lore.Document, lore.Document) {
	source, target := xrefNotionPages()
	source.Title = "Demo Log row"
	source.Body = "Demo completed."
	source.Refs[0].Instance = source.Source
	return source, target
}

type xrefSource struct {
	instance string
	docs     []lore.Document
}

func (s xrefSource) Name() string { return s.instance }

func (s xrefSource) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) {
		yield(lore.Batch{Docs: s.docs, Cursor: lore.Cursor{"fixture": "complete"}}, nil)
	}
}

func xrefSync(t *testing.T, store *sqlite.Store, docs ...lore.Document) {
	t.Helper()
	source := xrefSource{instance: docs[0].Source, docs: docs}
	syncer := NewSyncOrchestrator(store, []lore.Connector{source}, NewChunker(),
		leaseEmbedder{}, NewLinkResolver(store, nil), NewVectorSpace("fixture", "local", xrefDims))
	result, err := syncer.Sync(context.Background(), SyncOptions{})
	if err != nil || len(result.Failures) != 0 {
		t.Fatalf("Sync: result=%+v, error=%v", result, err)
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

func TestLinkResolverLinksANotionPageWrittenInAnotherURLForm(t *testing.T) {
	ctx := context.Background()
	store := xrefStore(t)
	source, target := xrefNotionPages()
	xrefIngest(t, store, source, target)

	if err := NewLinkResolver(store, nil).Link(ctx, []lore.Document{source, target}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	xrefAssertEdges(t, "corpus edges", xrefEdges(t, store, source.ID, target.ID), []entities.Edge{{
		Src:        source.ID,
		Dst:        target.ID,
		Kind:       entities.EdgeKindReferencesDoc,
		Confidence: 1.0,
	}})
	xrefAssertPending(t, "pending refs", xrefPending(t, store), nil)
}

func TestLinkResolverLinksANotionRowToItsRelatedPage(t *testing.T) {
	ctx := context.Background()
	store := xrefStore(t)
	row, target := xrefNotionRelationPages()
	xrefSync(t, store, row, target)

	xrefAssertEdges(t, "relation edges after sync", xrefEdges(t, store, row.ID), []entities.Edge{{
		Src: row.ID, Dst: target.ID, Kind: entities.EdgeKindReferencesDoc, Confidence: 1,
	}})
	xrefAssertPending(t, "pending relation refs", xrefPending(t, store), nil)

	stats, err := NewStatusService(store, "").Status(ctx)
	if err != nil || stats.Edges != 1 {
		t.Fatalf("Status: edges=%d, error=%v", stats.Edges, err)
	}
	trace, err := NewTraceService(store, nil).Trace(ctx, TraceRequest{Ref: row.URL, Direction: "out"})
	if err != nil {
		t.Fatalf("Trace row: %v", err)
	}
	if len(trace.Nodes) != 2 || !slices.ContainsFunc(trace.Nodes, func(node entities.EvidenceNode) bool {
		return node.Doc.ID == target.ID
	}) {
		t.Fatalf("Trace must include the related page: %v", trace.Nodes)
	}
}

func TestLinkResolverLinksAllThirtyRelationTargets(t *testing.T) {
	store := xrefStore(t)
	row, _ := xrefNotionRelationPages()
	row.Refs = nil
	docs := []lore.Document{row}
	var want []entities.Edge
	for i := 1; i <= 30; i++ {
		target := lore.Document{
			ID: lore.NewDocID("notion", lore.DocTypePage,
				fmt.Sprintf("90000000-0000-4000-8000-%012d", i)),
			Source: "notion", Type: lore.DocTypePage, Title: fmt.Sprintf("Session %d", i),
			Body: "Session notes.",
			URL:  fmt.Sprintf("https://app.notion.com/p/Session-%d-90000000000040008000%012d", i, i),
		}
		row.Refs = append(row.Refs, lore.RawRef{Kind: lore.RefKindURL, Instance: "notion",
			Value: fmt.Sprintf("https://www.notion.so/90000000000040008000%012d", i)})
		docs = append(docs, target)
		want = append(want, entities.Edge{Src: row.ID, Dst: target.ID,
			Kind: entities.EdgeKindReferencesDoc, Confidence: 1})
	}
	docs[0] = row
	xrefSync(t, store, docs...)
	xrefAssertEdges(t, "all relation edges after sync", xrefEdges(t, store, row.ID), want)
	xrefAssertPending(t, "pending relation refs", xrefPending(t, store), nil)
}

func TestLinkResolverKeepsARelationToAnUnindexedPagePending(t *testing.T) {
	ctx := context.Background()
	store := xrefStore(t)
	row, target := xrefNotionRelationPages()
	foreign := target
	foreign.Source = "notion-other"
	foreign.ID = lore.NewDocID(foreign.Source, lore.DocTypePage, "3e7a409f-814e-80a9-8644-fc0217b75f69")
	xrefIngest(t, store, foreign)
	xrefSync(t, store, row)

	wantPending := []entities.PendingRef{{SourceDoc: row.ID, Ref: row.Refs[0]}}
	xrefAssertEdges(t, "out-of-scope relation edges after sync", xrefEdges(t, store, row.ID), nil)
	xrefAssertPending(t, "pending relation refs", xrefPending(t, store), wantPending)
	if err := NewLinkResolver(store, nil).LinkPending(ctx); err != nil {
		t.Fatalf("LinkPending without own target: %v", err)
	}
	xrefAssertEdges(t, "out-of-scope relation edges after retry", xrefEdges(t, store, row.ID), nil)
	xrefAssertPending(t, "pending relation refs after retry", xrefPending(t, store), wantPending)

	xrefSync(t, store, target)
	xrefAssertEdges(t, "relation edges after own target sync", xrefEdges(t, store, row.ID), []entities.Edge{{
		Src: row.ID, Dst: target.ID, Kind: entities.EdgeKindReferencesDoc, Confidence: 1,
	}})
	xrefAssertPending(t, "pending relation refs after own target sync", xrefPending(t, store), nil)
}

func TestLinkResolverResolvesANotionLinkOnceItsPageArrives(t *testing.T) {
	ctx := context.Background()
	store := xrefStore(t)
	resolver := NewLinkResolver(store, nil)
	source, target := xrefNotionPages()
	xrefIngest(t, store, source)

	if err := resolver.Link(ctx, []lore.Document{source}); err != nil {
		t.Fatalf("Link before target ingestion: %v", err)
	}

	xrefAssertEdges(t, "edges before target ingestion", xrefEdges(t, store, source.ID, target.ID), nil)
	xrefAssertPending(t, "pending refs before target ingestion", xrefPending(t, store),
		[]entities.PendingRef{{SourceDoc: source.ID, Ref: source.Refs[0]}})

	xrefIngest(t, store, target)
	if err := resolver.LinkPending(ctx); err != nil {
		t.Fatalf("LinkPending after target ingestion: %v", err)
	}

	xrefAssertEdges(t, "edges after target ingestion", xrefEdges(t, store, source.ID, target.ID), []entities.Edge{{
		Src:        source.ID,
		Dst:        target.ID,
		Kind:       entities.EdgeKindReferencesDoc,
		Confidence: 1.0,
	}})
	xrefAssertPending(t, "pending refs after target ingestion", xrefPending(t, store), nil)
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

func TestLinkResolverKeepsTwoInstancesOfOneSourceApart(t *testing.T) {
	ctx := context.Background()
	store := xrefStore(t)

	var (
		corpus []lore.DocID
		docs   []lore.Document
	)
	for _, instance := range []string{xrefJiraEU, xrefJiraUS} {
		source := xrefInstanceTicket(instance, xrefSourceKey,
			"Blocked by "+xrefTicketKey+" on this deployment.",
			lore.RawRef{Kind: lore.RefKindTicketKey, Value: xrefTicketKey, Instance: instance})
		target := xrefInstanceTicket(instance, xrefTicketKey,
			"Signing in lands the user on the wrong tenant.")

		xrefIngest(t, store, source, target)
		corpus = append(corpus, source.ID, target.ID)
		docs = append(docs, source, target)
	}

	if err := NewLinkResolver(store, nil).Link(ctx, docs); err != nil {
		t.Fatalf("Link: %v", err)
	}

	xrefAssertEdges(t, "corpus edges", xrefEdges(t, store, corpus...), []entities.Edge{
		{
			Src:        lore.NewDocID(xrefJiraEU, lore.DocTypeTicket, xrefSourceKey),
			Dst:        lore.NewDocID(xrefJiraEU, lore.DocTypeTicket, xrefTicketKey),
			Kind:       entities.EdgeKindReferencesDoc,
			Confidence: 0.9,
		},
		{
			Src:        lore.NewDocID(xrefJiraUS, lore.DocTypeTicket, xrefSourceKey),
			Dst:        lore.NewDocID(xrefJiraUS, lore.DocTypeTicket, xrefTicketKey),
			Kind:       entities.EdgeKindReferencesDoc,
			Confidence: 0.9,
		},
	})
	xrefAssertPending(t, "pending refs", xrefPending(t, store), nil)
}

func TestLinkResolverKeepsAScopedRefPendingUntilItsOwnInstanceIsIndexed(t *testing.T) {
	ctx := context.Background()
	store := xrefStore(t)
	resolver := NewLinkResolver(store, nil)

	ref := lore.RawRef{Kind: lore.RefKindTicketKey, Value: xrefTicketKey, Instance: xrefJiraEU}
	source := xrefInstanceTicket(xrefJiraEU, xrefSourceKey,
		"Blocked by "+xrefTicketKey+" on this deployment.", ref)
	foreign := xrefInstanceTicket(xrefJiraUS, xrefTicketKey,
		"Signing in lands the user on the wrong tenant.")
	own := xrefInstanceTicket(xrefJiraEU, xrefTicketKey,
		"Signing in lands the user on the wrong tenant.")

	xrefIngest(t, store, source, foreign)
	if err := resolver.Link(ctx, []lore.Document{source, foreign}); err != nil {
		t.Fatalf("round 1 Link: %v", err)
	}

	corpus := []lore.DocID{source.ID, foreign.ID, own.ID}
	xrefAssertEdges(t, "round 1 edges", xrefEdges(t, store, corpus...), nil)
	xrefAssertPending(t, "round 1 pending refs", xrefPending(t, store),
		[]entities.PendingRef{{SourceDoc: source.ID, Ref: ref}})

	xrefIngest(t, store, own)
	if err := resolver.LinkPending(ctx); err != nil {
		t.Fatalf("round 2 LinkPending: %v", err)
	}

	xrefAssertEdges(t, "round 2 edges", xrefEdges(t, store, corpus...), []entities.Edge{{
		Src:        source.ID,
		Dst:        own.ID,
		Kind:       entities.EdgeKindReferencesDoc,
		Confidence: 0.9,
	}})
	xrefAssertPending(t, "round 2 pending refs", xrefPending(t, store), nil)
}
