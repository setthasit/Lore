// Package e2e drives the whole pipeline end to end: a fixture GitHub API, the
// real connector, the real chunker, a real SQLite index in a temporary
// directory, and the real retrieval path. Only two things are stood in for — the
// network, by an httptest server replaying hand-written fixtures, and the
// embedder, by a deterministic local one — so what the tests here prove is that
// the seams between the layers line up, which no single package's tests can say.
package e2e

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode"

	"lore/internal/connectors/embedder"
	"lore/internal/connectors/github"
	"lore/internal/entities"
	"lore/internal/repositories/sqlite"
	"lore/internal/services"
)

const (
	// fixtureToken is not a credential. The fixture API asserts the connector
	// sent it in the Authorization header, which is the only place it belongs.
	fixtureToken = "ghp_e2e_fixture_token"

	fixtureRepo = "acme/lore"

	// fixtureHost is the origin the fixtures are written against. Every fixture
	// is rewritten to the test server's own origin as it is served, so the URLs
	// a bundle cites point at the API the documents were read from — which is
	// what makes "every node carries a real URL" checkable from outside.
	fixtureHost = "https://github.example"

	// question is the decision question the fixture pull request answers.
	question = "why did we pick sqlite"

	// fixtureDocuments is what the fixtures normalize to: one commit, the pull
	// request, its review and review comment, the issue and its comment.
	fixtureDocuments = 6

	// topK is the retrieval width per strategy, mirroring config.DefaultTopK.
	topK = 12
)

// prDocID is the pull request that argues for SQLite: the document the question
// has to retrieve first.
var prDocID = entities.NewDocID("github", entities.DocTypePR, fixtureRepo+"/pull/42")

// --- fake embedder ---------------------------------------------------------

// fakeDims is deliberately tiny: the tests read ranking, not embedding quality,
// and a narrow vector keeps the fixture index cheap to build.
const fakeDims = 8

// fakeEmbedder stands in for a hosted embedder without leaving the process. It
// hashes a text's tokens into a bag of words over fakeDims dimensions and
// returns the L2-normalized result, so identical text always yields an identical
// vector and texts sharing vocabulary land near each other.
type fakeEmbedder struct{}

var _ embedder.Embedder = fakeEmbedder{}

func (fakeEmbedder) Identity() string {
	return embedder.FormatIdentity("fake", "bag-of-words", fakeDims)
}

func (fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vectors[i] = bagOfWords(text)
	}
	return vectors, nil
}

// bagOfWords projects text's tokens onto fakeDims dimensions by hashing each
// one, then normalizes. Every vector is a unit vector, which is what makes the
// store's L2 ranking order hits the way cosine similarity would.
func bagOfWords(text string) []float32 {
	vector := make([]float32, fakeDims)
	for _, token := range strings.FieldsFunc(strings.ToLower(text), isTokenBreak) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(token))
		vector[h.Sum32()%fakeDims]++
	}

	var sum float64
	for _, v := range vector {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		// Text with no indexable token still has to be a unit vector, so it
		// keeps a defined distance to everything else.
		vector[0] = 1
		return vector
	}

	norm := float32(math.Sqrt(sum))
	for i := range vector {
		vector[i] /= norm
	}
	return vector
}

func isTokenBreak(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }

// --- fixture API -----------------------------------------------------------

// operationPattern lifts the operation name out of a GraphQL query, which is how
// the fixture API decides what to answer.
var operationPattern = regexp.MustCompile(`query\s+(\w+)`)

// opFixtures maps a GraphQL operation to the response it gets. Every connection
// in the fixtures is a single page, so the connector never asks for a follow-up
// page: an operation missing here means a fixture's pageInfo promises a page
// that does not exist, and the fixture API says so instead of guessing.
var opFixtures = map[string]string{
	"LoreCommits":      "commits_page1.json",
	"LorePullRequests": "prs_page1.json",
	"LoreIssues":       "issues_page1.json",
}

const (
	restCommitPath = "/repos/" + fixtureRepo + "/commits/"

	// restCommitOp is the call-counter key for the connector's one REST call,
	// the touched-file list GraphQL does not expose.
	restCommitOp = "REST commit"
)

// fixtureAPI replays the fixtures as a GitHub API would, and counts what it was
// asked for so a test can assert the connector really went through it.
type fixtureAPI struct {
	t      *testing.T
	server *httptest.Server

	mu    sync.Mutex
	calls map[string]int
}

func newFixtureAPI(t *testing.T) *fixtureAPI {
	t.Helper()

	api := &fixtureAPI{t: t, calls: make(map[string]int)}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.server.Close)
	return api
}

func (a *fixtureAPI) serve(w http.ResponseWriter, r *http.Request) {
	if auth := r.Header.Get("Authorization"); !strings.Contains(auth, fixtureToken) {
		a.t.Errorf("%s: Authorization header does not carry the token", r.URL.Path)
	}
	if r.URL.Path != "/graphql" {
		a.serveREST(w, r)
		return
	}

	var req struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.reject(w, "decode graphql request: %v", err)
		return
	}
	match := operationPattern.FindStringSubmatch(req.Query)
	if match == nil {
		a.reject(w, "graphql query carries no operation name: %s", req.Query)
		return
	}
	fixture, ok := opFixtures[match[1]]
	if !ok {
		a.reject(w, "no fixture for graphql operation %q", match[1])
		return
	}

	a.record(match[1])
	a.write(w, fixture)
}

func (a *fixtureAPI) serveREST(w http.ResponseWriter, r *http.Request) {
	oid, ok := strings.CutPrefix(r.URL.Path, restCommitPath)
	if !ok || len(oid) < 7 {
		a.reject(w, "unexpected REST path %q", r.URL.Path)
		return
	}

	a.record(restCommitOp)
	a.write(w, "rest_commit_"+oid[:7]+".json")
}

// write serves a fixture with its placeholder origin rewritten to this server's,
// so the documents cite the API they came from.
func (a *fixtureAPI) write(w http.ResponseWriter, name string) {
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		a.reject(w, "read fixture %s: %v", name, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	rewritten := strings.ReplaceAll(string(body), fixtureHost, a.server.URL)
	if _, err := w.Write([]byte(rewritten)); err != nil {
		a.t.Errorf("write fixture %s: %v", name, err)
	}
}

// reject answers with a status the connector does not retry, so a fixture
// mistake surfaces as one failing test instead of a minute of backoff.
func (a *fixtureAPI) reject(w http.ResponseWriter, format string, args ...any) {
	a.t.Errorf(format, args...)
	w.WriteHeader(http.StatusBadRequest)
}

func (a *fixtureAPI) record(op string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls[op]++
}

func (a *fixtureAPI) callCount(op string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[op]
}

// --- workspace -------------------------------------------------------------

// workspace is the pipeline wired the way the injector wires it, over a fixture
// API and a fresh index file.
type workspace struct {
	api    *fixtureAPI
	round  services.SyncOrchestrator
	query  services.QueryService
	status services.StatusService
}

func newWorkspace(t *testing.T) *workspace {
	t.Helper()

	api := newFixtureAPI(t)

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "workspace.db"), fakeDims)
	if err != nil {
		t.Fatalf("open the workspace index: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close the workspace index: %v", err)
		}
	})

	emb := fakeEmbedder{}
	connectors := []entities.Connector{
		github.NewConnector(fixtureToken, []string{fixtureRepo}, api.server.URL),
	}

	return &workspace{
		api:    api,
		round:  services.NewSyncOrchestrator(store, connectors, services.NewChunker(), emb),
		query:  services.NewQueryService(store, emb, topK),
		status: services.NewStatusService(store),
	}
}

func (w *workspace) sync(ctx context.Context, t *testing.T, what string) {
	t.Helper()

	if err := w.round.Sync(ctx, services.SyncOptions{}); err != nil {
		t.Fatalf("%s sync: %v", what, err)
	}
}

func (w *workspace) stats(ctx context.Context, t *testing.T) entities.IndexStats {
	t.Helper()

	stats, err := w.status.Status(ctx)
	if err != nil {
		t.Fatalf("read the index's state: %v", err)
	}
	return stats
}

func (w *workspace) ask(ctx context.Context, t *testing.T) *entities.EvidenceBundle {
	t.Helper()

	bundle, err := w.query.FindDecision(ctx, services.FindDecisionRequest{Question: question})
	if err != nil {
		t.Fatalf("find_decision %q: %v", question, err)
	}
	if bundle == nil {
		t.Fatalf("find_decision %q returned no bundle", question)
	}
	return bundle
}

// --- tests -----------------------------------------------------------------

// The whole point of the stack, asserted once from the outside: documents pulled
// from a fixture API through the connector, chunked, embedded and indexed, come
// back as cited evidence for a question nobody told the index about.
func TestSyncedFixtureRepositoryAnswersItsDecisionQuestion(t *testing.T) {
	ctx := context.Background()
	w := newWorkspace(t)

	w.sync(ctx, t, "first")

	for _, op := range []string{"LoreCommits", "LorePullRequests", "LoreIssues", restCommitOp} {
		if w.api.callCount(op) == 0 {
			t.Errorf("the fixture API was never asked for %s", op)
		}
	}

	stats := w.stats(ctx, t)
	if stats.Documents != fixtureDocuments {
		t.Errorf("indexed documents = %d, want %d", stats.Documents, fixtureDocuments)
	}
	if stats.Chunks < stats.Documents {
		t.Errorf("indexed chunks = %d, want at least one per document (%d)", stats.Chunks, stats.Documents)
	}
	if len(stats.Cursors) != 1 || stats.Cursors[0].Connector != "github" {
		t.Errorf("checkpointed connectors = %+v, want one entry for github", stats.Cursors)
	}

	bundle := w.ask(ctx, t)

	if bundle.Question != question {
		t.Errorf("bundle question = %q, want %q", bundle.Question, question)
	}
	if bundle.Anchor.Kind != entities.AnchorQuery {
		t.Errorf("anchor kind = %d, want AnchorQuery (%d)", bundle.Anchor.Kind, entities.AnchorQuery)
	}
	if bundle.Anchor.Query != question {
		t.Errorf("anchor query = %q, want %q", bundle.Anchor.Query, question)
	}
	// The graph walk lands with the edges wave; until then a bundle carries seed
	// nodes only, and claiming otherwise here would hide its arrival.
	if len(bundle.Chains) != 0 || len(bundle.Gaps) != 0 {
		t.Errorf("bundle carries %d chains and %d gaps, want none of either",
			len(bundle.Chains), len(bundle.Gaps))
	}

	if len(bundle.Nodes) == 0 {
		t.Fatalf("bundle cites nothing for %q", question)
	}
	for _, node := range bundle.Nodes {
		if !strings.HasPrefix(node.Doc.URL, w.api.server.URL) {
			t.Errorf("node %s cites %q, want a URL on the fixture host %s",
				node.Doc.ID, node.Doc.URL, w.api.server.URL)
		}
		if strings.TrimSpace(node.Excerpt) == "" {
			t.Errorf("node %s carries no excerpt", node.Doc.ID)
		}
		if node.Role != entities.RoleSeed {
			t.Errorf("node %s has role %q, want %q", node.Doc.ID, node.Role, entities.RoleSeed)
		}
	}

	top := bundle.Nodes[0]
	if top.Doc.ID != prDocID {
		t.Errorf("best evidence is %s, want the pull request that argues for SQLite (%s)", top.Doc.ID, prDocID)
	}
	if !strings.Contains(strings.ToLower(top.Excerpt), "picked sqlite") {
		t.Errorf("best excerpt does not quote the decision: %q", top.Excerpt)
	}
}

// A second round over unchanged sources is a no-op through every layer: the
// connector replays nothing the cursor already covers, upserts and chunk
// replacement are idempotent, and the answer is the same answer.
func TestSecondSyncOverUnchangedFixturesChangesNothing(t *testing.T) {
	ctx := context.Background()
	w := newWorkspace(t)

	w.sync(ctx, t, "first")
	before, beforeBundle := w.stats(ctx, t), w.ask(ctx, t)

	w.sync(ctx, t, "second")
	after, afterBundle := w.stats(ctx, t), w.ask(ctx, t)

	if after.Documents != before.Documents || after.Chunks != before.Chunks {
		t.Errorf("index holds %d documents and %d chunks after the second sync, want %d and %d",
			after.Documents, after.Chunks, before.Documents, before.Chunks)
	}

	if len(afterBundle.Nodes) != len(beforeBundle.Nodes) {
		t.Fatalf("bundle cites %d documents after the second sync, want %d",
			len(afterBundle.Nodes), len(beforeBundle.Nodes))
	}
	for i, node := range afterBundle.Nodes {
		want := beforeBundle.Nodes[i]
		if node.Doc.ID != want.Doc.ID || node.Excerpt != want.Excerpt {
			t.Errorf("node %d after the second sync is %s / %q, want %s / %q",
				i, node.Doc.ID, node.Excerpt, want.Doc.ID, want.Excerpt)
		}
	}
}
