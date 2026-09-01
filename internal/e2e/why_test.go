package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lore/internal/connectors/github"
	"lore/internal/connectors/gitrepo"
	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/services"
)

const (
	whyFixtures = "why"

	whyFile = "internal/ingest/writer.go"

	whyLineStart = 3
	whyLineEnd   = 13

	whyQuestion = "why does the ingest writer own a bounded queue"

	whyDocuments = 4

	whyRemote = githubSource + ":" + fixtureRepo

	cloneEmail = "fixture@example.invalid"

	// The width the index stores a commit SHA at, so a gap names a shortened one.
	indexedSHAChars = 12

	codeAnchorRefusal = "no repositories registered — code anchoring disabled for this workspace"
)

var (
	whyPRDocID    = entities.NewDocID(githubSource, entities.DocTypePR, fixtureRepo+"/pull/91")
	whyIssueDocID = entities.NewDocID(githubSource, entities.DocTypeIssue, fixtureRepo+"/issues/88")
)

// Line 3 opens the span, line 4 holds the field the unsynced commit renames and
// line 13 the sentinel the follow-up renames, so one span blames three commits.
const writerAtFirstCommit = `package ingest

type Writer struct {
	batches chan batch
	done    chan struct{}
}

func (w *Writer) Enqueue(b batch) error {
	select {
	case w.batches <- b:
		return nil
	case <-w.done:
		return errClosed
	}
}
`

type clone struct {
	t    *testing.T
	root string
	home string
}

func newClone(t *testing.T) *clone {
	t.Helper()

	c := &clone{t: t, root: t.TempDir(), home: t.TempDir()}
	c.git("init", "--initial-branch=main", "--quiet")
	return c
}

func (c *clone) git(args ...string) string { return c.gitWithEnv(nil, args...) }

func (c *clone) gitWithEnv(extra []string, args ...string) string {
	c.t.Helper()

	cmd := exec.Command("git", append([]string{"-C", c.root}, args...)...)
	cmd.Env = append([]string{
		"LC_ALL=C",
		"HOME=" + c.home,
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
	}, extra...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (c *clone) commit(file, content, author, when, message string) string {
	c.t.Helper()

	full := filepath.Join(c.root, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		c.t.Fatalf("mkdir for %s: %v", file, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		c.t.Fatalf("write %s: %v", file, err)
	}

	c.git("add", "-A")
	c.gitWithEnv(
		[]string{"GIT_AUTHOR_DATE=" + when, "GIT_COMMITTER_DATE=" + when},
		"-c", "user.name="+author, "-c", "user.email="+cloneEmail,
		"commit", "--quiet", "-m", message,
	)

	return strings.TrimSpace(c.git("rev-parse", "HEAD"))
}

// The fixture corpus carries the SHAs this clone produced, so a blamed SHA resolves.
type codeCorpus struct {
	repos    []services.CodeRepo
	fixtures string
	anchor   string
	followUp string
	unsynced string
}

func newCodeCorpus(t *testing.T) codeCorpus {
	t.Helper()

	c := newClone(t)
	anchor := c.commit(whyFile, writerAtFirstCommit, "Ada Lovelace", "2024-05-06T09:00:00Z",
		"Put a bounded queue in front of the ingest writer")

	renamedSentinel := strings.Replace(writerAtFirstCommit, "errClosed", "errWriterClosed", 1)
	followUp := c.commit(whyFile, renamedSentinel, "Grace Hopper", "2024-05-08T10:00:00Z",
		"Rename the writer's closed sentinel\n\nFollows up "+anchor+".")

	renamedField := strings.ReplaceAll(renamedSentinel, "batches", "pending")
	unsynced := c.commit(whyFile, renamedField, "Ada Lovelace", "2024-05-20T08:00:00Z",
		"Say what the writer's queue field holds")

	return codeCorpus{
		repos:    []services.CodeRepo{{Path: c.root, Remote: whyRemote, Git: gitrepo.New(c.root)}},
		fixtures: materialiseCorpus(t, map[string]string{"ANCHOR_SHA": anchor, "FOLLOWUP_SHA": followUp}),
		anchor:   anchor,
		followUp: followUp,
		unsynced: unsynced,
	}
}

// The corpus ships as a template: its SHAs exist only once the clone is built,
// and serveREST looks a commit fixture up by the leading characters of its SHA.
func materialiseCorpus(t *testing.T, shas map[string]string) string {
	t.Helper()

	templates := corpusDir(whyFixtures)
	entries, err := os.ReadDir(templates)
	if err != nil {
		t.Fatalf("read the %s corpus templates: %v", whyFixtures, err)
	}

	bodyPairs := make([]string, 0, 2*len(shas))
	namePairs := make([]string, 0, 2*len(shas))
	for placeholder, sha := range shas {
		bodyPairs = append(bodyPairs, placeholder, sha)
		namePairs = append(namePairs, placeholder, sha[:restSHAChars])
	}
	body, name := strings.NewReplacer(bodyPairs...), strings.NewReplacer(namePairs...)

	dir := t.TempDir()
	for _, entry := range entries {
		template, err := os.ReadFile(filepath.Join(templates, entry.Name()))
		if err != nil {
			t.Fatalf("read the fixture template %s: %v", entry.Name(), err)
		}
		materialised := filepath.Join(dir, name.Replace(entry.Name()))
		if err := os.WriteFile(materialised, []byte(body.Replace(string(template))), 0o600); err != nil {
			t.Fatalf("write the materialised fixture %s: %v", materialised, err)
		}
	}
	return dir
}

func whyWorkspace(ctx context.Context, t *testing.T, fixtures string, repos []services.CodeRepo) *workspace {
	t.Helper()

	api := newFixtureAPI(t, fixtures, fixtureHost)
	api.listen(api.serve)

	w := newIndexedWorkspace(t, api, []entities.Connector{
		github.NewConnector(fixtureToken, []string{fixtureRepo}, api.server.URL),
	}, repos)
	w.sync(ctx, t, whyFixtures)

	if stats := w.stats(ctx, t); stats.Documents != whyDocuments {
		t.Fatalf("indexed documents = %d, want %d: the corpus is two commits, their pull request and the issue it closes",
			stats.Documents, whyDocuments)
	}
	return w
}

func whyOfTheBlamedSpan(ctx context.Context, w *workspace) (*entities.EvidenceBundle, error) {
	return w.why.Why(ctx, services.WhyRequest{
		Repo:      whyRemote,
		File:      whyFile,
		LineStart: whyLineStart,
		LineEnd:   whyLineEnd,
		Question:  whyQuestion,
	})
}

func commitDocID(sha string) entities.DocID {
	return entities.NewDocID(githubSource, entities.DocTypeCommit, fixtureRepo+"/commit/"+sha)
}

func TestWhyAnchorsABlamedSpanOnTheCommitsPullRequestAndIssueBehindIt(t *testing.T) {
	ctx := context.Background()
	corpus := newCodeCorpus(t)
	w := whyWorkspace(ctx, t, corpus.fixtures, corpus.repos)

	bundle, err := whyOfTheBlamedSpan(ctx, w)
	if err != nil {
		t.Fatalf("why %s:%d-%d: %v", whyFile, whyLineStart, whyLineEnd, err)
	}
	if bundle.Question != whyQuestion {
		t.Errorf("bundle question = %q, want the question as asked %q", bundle.Question, whyQuestion)
	}

	assertCodeAnchor(t, bundle.Anchor, corpus)
	assertWhyCitations(t, w, bundle, corpus)
	assertBlamedExcerpts(t, bundle, corpus)

	run := []entities.DocID{commitDocID(corpus.anchor), whyPRDocID, whyIssueDocID}
	if !slices.ContainsFunc(bundle.Chains, func(chain []entities.DocID) bool { return chainRuns(chain, run) }) {
		t.Errorf("no chain walks %v end to end; chains: %v", run, bundle.Chains)
	}

	wantGaps := []string{
		"trail ends at commit " + corpus.unsynced[:indexedSHAChars] + ", not synced from a source",
	}
	if !slices.Equal(bundle.Gaps, wantGaps) {
		t.Errorf("gaps = %q, want exactly %q", bundle.Gaps, wantGaps)
	}

	assertBundleContract(t, "why", bundle)
}

func TestWhyWithoutARegisteredCloneRefusesTheSpanItCouldOtherwiseBlame(t *testing.T) {
	ctx := context.Background()
	corpus := newCodeCorpus(t)
	w := whyWorkspace(ctx, t, corpus.fixtures, nil)

	bundle, err := whyOfTheBlamedSpan(ctx, w)
	if bundle != nil {
		t.Errorf("why returned %+v, want no bundle: an empty bundle would read as no evidence found", bundle)
	}
	if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
		t.Fatalf("why error kind = %s, want %s (error %v)", got, internalerror.KindPrecondition, err)
	}
	if err.Error() != codeAnchorRefusal {
		t.Errorf("why error = %q, want %q", err, codeAnchorRefusal)
	}
}

func assertCodeAnchor(t *testing.T, anchor entities.Anchor, corpus codeCorpus) {
	t.Helper()

	if anchor.Kind != entities.AnchorCodeSpan {
		t.Errorf("anchor kind = %d, want AnchorCodeSpan (%d)", anchor.Kind, entities.AnchorCodeSpan)
	}
	code := anchor.Code
	if code == nil {
		t.Fatalf("anchor carries no code span: %+v", anchor)
	}
	if code.Repo != whyRemote || code.File != whyFile {
		t.Errorf("anchor grounds %s %s, want %s %s", code.Repo, code.File, whyRemote, whyFile)
	}
	if code.LineStart != whyLineStart || code.LineEnd != whyLineEnd {
		t.Errorf("anchor spans %d-%d, want %d-%d", code.LineStart, code.LineEnd, whyLineStart, whyLineEnd)
	}

	want := []string{corpus.anchor, corpus.unsynced, corpus.followUp}
	if !slices.Equal(code.BlamedSHAs, want) {
		t.Errorf("blamed SHAs = %v, want %v in first-blamed order", code.BlamedSHAs, want)
	}
}

func assertWhyCitations(t *testing.T, w *workspace, bundle *entities.EvidenceBundle, corpus codeCorpus) {
	t.Helper()

	wanted := []struct {
		id   entities.DocID
		role string
		path string
	}{
		{commitDocID(corpus.anchor), entities.RoleBlamedCommit, "/commit/" + corpus.anchor},
		{commitDocID(corpus.followUp), entities.RoleBlamedCommit, "/commit/" + corpus.followUp},
		{whyPRDocID, entities.RoleLinkedChange, "/pull/91"},
		{whyIssueDocID, entities.RoleLinkedTicket, "/issues/88"},
	}
	if len(bundle.Nodes) != len(wanted) {
		t.Errorf("bundle cites %d nodes %v, want %d", len(bundle.Nodes), citedIDs(bundle), len(wanted))
	}
	for _, want := range wanted {
		node, cited := citedNode(bundle, want.id)
		if !cited {
			t.Errorf("bundle does not cite %s; it cites %v", want.id, citedIDs(bundle))
			continue
		}
		if node.Role != want.role {
			t.Errorf("%s carries role %q, want %q", want.id, node.Role, want.role)
		}
		if url := w.api.server.URL + "/" + fixtureRepo + want.path; node.Doc.URL != url {
			t.Errorf("%s carries URL %q, want %q", want.id, node.Doc.URL, url)
		}
	}
}

// The excerpt text lives only in the clone: no fixture carries it.
func assertBlamedExcerpts(t *testing.T, bundle *entities.EvidenceBundle, corpus codeCorpus) {
	t.Helper()

	owned := []struct {
		id   entities.DocID
		line string
	}{
		{commitDocID(corpus.anchor), "type Writer struct {"},
		{commitDocID(corpus.followUp), "return errWriterClosed"},
	}
	for _, want := range owned {
		node, cited := citedNode(bundle, want.id)
		if !cited {
			continue
		}
		if !strings.Contains(node.Excerpt, want.line) {
			t.Errorf("%s excerpts %q, which does not carry the line %q it owns",
				want.id, node.Excerpt, want.line)
		}
	}
}

func chainRuns(chain, run []entities.DocID) bool {
	for i := range len(chain) - len(run) + 1 {
		if slices.Equal(chain[i:i+len(run)], run) {
			return true
		}
	}
	return false
}
