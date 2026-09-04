package services

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/connectors/gitrepo"
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
	xrefFile       = "internal/auth/auth.go"
	xrefAuthor     = "Ada Lovelace"
	xrefEmail      = "ada@example.invalid"
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

type xrefClone struct {
	t    *testing.T
	root string
}

func newXrefClone(t *testing.T) *xrefClone {
	t.Helper()

	c := &xrefClone{t: t, root: t.TempDir()}
	c.git(nil, "init", "--initial-branch=main", "--quiet")

	return c
}

func (c *xrefClone) git(dates []string, args ...string) string {
	c.t.Helper()

	cmd := exec.Command("git", append([]string{"-C", c.root}, args...)...)
	cmd.Env = append([]string{
		"LC_ALL=C",
		"HOME=" + c.t.TempDir(),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
	}, dates...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return string(out)
}

func (c *xrefClone) commit(content, when, subject string) string {
	c.t.Helper()

	full := filepath.Join(c.root, filepath.FromSlash(xrefFile))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		c.t.Fatalf("mkdir for %s: %v", xrefFile, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		c.t.Fatalf("write %s: %v", xrefFile, err)
	}

	c.git(nil, "add", "-A")
	c.git([]string{"GIT_AUTHOR_DATE=" + when, "GIT_COMMITTER_DATE=" + when},
		"-c", "user.name="+xrefAuthor, "-c", "user.email="+xrefEmail,
		"commit", "--quiet", "-m", subject)

	return strings.TrimSpace(c.git(nil, "rev-parse", "HEAD"))
}

func xrefCommitDocID(sha string) lore.DocID {
	return lore.NewDocID("github", lore.DocTypeCommit, xrefSlug+"/commit/"+sha)
}

func xrefCommitDoc(sha, subject string) lore.Document {
	return lore.Document{
		ID:      xrefCommitDocID(sha),
		Source:  "github",
		Type:    lore.DocTypeCommit,
		RepoRef: "github:" + xrefSlug,
		Title:   subject,
		Body:    subject,
		Author:  xrefAuthor,
		URL:     "https://github.com/" + xrefSlug + "/commit/" + sha,
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

func TestLinkResolverAnchorsAPathToEveryCommitThatTouchedIt(t *testing.T) {
	ctx := context.Background()
	store := xrefStore(t)

	clone := newXrefClone(t)
	const addSubject = "Give the auth package a tenant check"
	const fixSubject = "Read the tenant from the session, not the URL"
	added := clone.commit("package auth\n\nfunc Check(t string) bool { return t != \"\" }\n",
		"2024-05-06T09:00:00Z", addSubject)
	fixed := clone.commit("package auth\n\nfunc Check(s Session) bool { return s.Tenant != \"\" }\n",
		"2024-05-08T10:00:00Z", fixSubject)

	page := lore.Document{
		ID:     xrefPageID,
		Source: "notion",
		Type:   lore.DocTypePage,
		Title:  "Auth rollout decision",
		Body:   "The tenant check lives in " + xrefFile + ".",
		Author: "dana",
		URL:    "https://notion.so/design/auth-rollout",
		Refs:   []lore.RawRef{{Kind: lore.RefKindFilePath, Value: xrefFile}},
	}
	xrefIngest(t, store, page, xrefCommitDoc(added, addSubject), xrefCommitDoc(fixed, fixSubject))

	repos := []CodeRepo{{Path: clone.root, Remote: "github:" + xrefSlug, Git: gitrepo.New(clone.root)}}
	if err := NewLinkResolver(store, repos).Link(ctx, []lore.Document{page}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	want := []entities.Edge{
		{Src: xrefPageID, Dst: xrefCommitDocID(added), Kind: entities.EdgeKindMentionsPath, Confidence: 0.7},
		{Src: xrefPageID, Dst: xrefCommitDocID(fixed), Kind: entities.EdgeKindMentionsPath, Confidence: 0.7},
	}
	slices.SortFunc(want, walkEdgeOrder)
	xrefAssertEdges(t, "corpus edges", xrefEdges(t, store, xrefPageID), want)
	xrefAssertPending(t, "pending refs", xrefPending(t, store), nil)

	inbound, err := store.Neighbors(ctx, []lore.DocID{xrefCommitDocID(fixed)}, nil, entities.DirIn)
	if err != nil {
		t.Fatalf("Neighbors in: %v", err)
	}
	xrefAssertEdges(t, "edges into the newer commit", inbound, []entities.Edge{
		{Src: xrefPageID, Dst: xrefCommitDocID(fixed), Kind: entities.EdgeKindMentionsPath, Confidence: 0.7},
	})
}
