package e2e

import (
	"cmp"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/repositories/sqlite"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/plugins/code/git"
	"github.com/setthasit/Lore/sdk"
)

// The path-to-commit anchor is the one link the resolver cannot draw without a
// real repository behind it, so this test needs the git code plugin. The engine
// must not know a plugin by name, which leaves it here: test/e2e composes the
// plugins with the services exactly as the binary does.
const (
	xrefDims = 3
	xrefSlug = "acme/lore"

	xrefFile   = "internal/auth/auth.go"
	xrefAuthor = "Ada Lovelace"
	xrefEmail  = "ada@example.invalid"
)

var xrefPageID = lore.NewDocID("notion", lore.DocTypePage, "design/auth-rollout")

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

// The store answers in whatever order its query plan produces, so both sides of
// a comparison are ordered by the identity of an edge before they are compared.
func xrefEdgeOrder(a, b entities.Edge) int {
	return cmp.Or(cmp.Compare(a.Src, b.Src), cmp.Compare(a.Dst, b.Dst), cmp.Compare(a.Kind, b.Kind))
}

func xrefAssertEdges(t *testing.T, what string, got, want []entities.Edge) {
	t.Helper()

	slices.SortFunc(got, xrefEdgeOrder)
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

	repos := []services.CodeRepo{{Path: clone.root, Remote: "github:" + xrefSlug, Git: git.New(clone.root)}}
	if err := services.NewLinkResolver(store, repos).Link(ctx, []lore.Document{page}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	want := []entities.Edge{
		{Src: xrefPageID, Dst: xrefCommitDocID(added), Kind: entities.EdgeKindMentionsPath, Confidence: 0.7},
		{Src: xrefPageID, Dst: xrefCommitDocID(fixed), Kind: entities.EdgeKindMentionsPath, Confidence: 0.7},
	}
	slices.SortFunc(want, xrefEdgeOrder)
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
