package gitlab

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
)

const (
	// Obviously not a credential; the stub asserts it reaches the PRIVATE-TOKEN
	// header and nowhere else.
	fakeToken = "glpat-fake-token-value"

	projectPath = "acme/widgets"
	apiPrefix   = "/api/v4/projects/acme%2Fwidgets"

	// The fixtures' own web host, which is not the stub's: a document cites the
	// URL GitLab reported, not one the connector guessed.
	fixtureWeb = "https://gitlab.example.invalid/acme/widgets"

	sha1 = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	sha2 = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"
	sha3 = "bb44cc55dd66ee77ff8899aabbccddeeff001122"
)

// Documents the fixtures are expected to produce, in stream order.
const (
	commit1ID  lore.DocID = "gitlab:commit:" + projectPath + "/commit/" + sha1
	mr7ID      lore.DocID = "gitlab:pr:" + projectPath + "/pull/7"
	review7ID  lore.DocID = "gitlab:pr_review:" + projectPath + "/pull/7#note_1001"
	reply7ID   lore.DocID = "gitlab:review_comment:" + projectPath + "/pull/7#note_1002"
	single7ID  lore.DocID = "gitlab:review_comment:" + projectPath + "/pull/7#note_1003"
	commit2ID  lore.DocID = "gitlab:commit:" + projectPath + "/commit/" + sha2
	issue12ID  lore.DocID = "gitlab:issue:" + projectPath + "/issues/12"
	note12ID   lore.DocID = "gitlab:issue_comment:" + projectPath + "/issues/12#note_2001"
	mr8ID      lore.DocID = "gitlab:pr:" + projectPath + "/pull/8"
	single8ID  lore.DocID = "gitlab:review_comment:" + projectPath + "/pull/8#note_1010"
	commit3ID  lore.DocID = "gitlab:commit:" + projectPath + "/commit/" + sha3
	repoRefAll            = "gitlab:" + projectPath
)

func wantBatchedIDs() [][]lore.DocID {
	return [][]lore.DocID{
		{commit1ID, mr7ID, review7ID, reply7ID, single7ID},
		{commit2ID, issue12ID, note12ID},
		{mr8ID, single8ID},
		{commit3ID},
	}
}

func wantCursors() []lore.Cursor {
	return []lore.Cursor{
		{projectPath + ":updated_at": "2024-05-01T09:30:00Z", projectPath + ":doc_id": string(mr7ID)},
		{projectPath + ":updated_at": "2024-05-02T15:00:00Z", projectPath + ":doc_id": string(issue12ID)},
		{projectPath + ":updated_at": "2024-05-03T12:00:00Z", projectPath + ":doc_id": string(mr8ID)},
		{projectPath + ":updated_at": "2024-05-04T08:00:00Z", projectPath + ":doc_id": string(commit3ID)},
	}
}

// stub replays hand-written fixtures shaped like real GitLab REST responses.
// hook, when set, may answer a request itself — that is how failure modes are
// injected.
type stub struct {
	t      *testing.T
	server *httptest.Server
	hook   func(r *http.Request, w http.ResponseWriter) bool

	mu      sync.Mutex
	calls   map[string]int
	token   string
	queries map[string][]string
	waited  []time.Duration
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{t: t, calls: make(map[string]int), queries: make(map[string][]string)}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

// Batches close after two documents so boundaries are observable; retries record
// their wait instead of taking it.
func (s *stub) connector(opts ...Option) *Connector {
	return s.connectorFor([]string{projectPath}, opts...)
}

func (s *stub) connectorFor(projects []string, opts ...Option) *Connector {
	base := []Option{withBatchSize(2), withMaxAttempts(3), withBackoff(time.Millisecond)}
	c := NewConnector(fakeToken, projects, s.server.URL, append(base, opts...)...)
	c.client.sleep = s.wait
	return c
}

// route names the collection a path addresses, and the fixture that answers it.
func route(r *http.Request) (op, fixture string, ok bool) {
	rest, found := strings.CutPrefix(r.URL.EscapedPath(), apiPrefix)
	if !found {
		return "", "", false
	}
	page := r.URL.Query().Get("page")

	switch {
	case rest == "/repository/commits":
		return "commits", "commits_page" + page + ".json", true
	case strings.HasPrefix(rest, "/repository/commits/") && strings.HasSuffix(rest, "/diff"):
		sha := strings.TrimSuffix(strings.TrimPrefix(rest, "/repository/commits/"), "/diff")
		return "commit diff", "commit_diff_" + sha + ".json", true
	case rest == "/merge_requests":
		return "merge requests", "merge_requests_page" + page + ".json", true
	case rest == "/issues":
		return "issues", "issues_page" + page + ".json", true
	}

	for prefix, kind := range map[string]string{"/merge_requests/": "mr", "/issues/": "issue"} {
		nested, found := strings.CutPrefix(rest, prefix)
		if !found {
			continue
		}
		if iid, tail, split := strings.Cut(nested, "/"); split && tail != "" {
			return kind + " " + tail, kind + "_" + iid + "_" + tail + ".json", true
		}
	}
	return "", "", false
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.t.Errorf("%s %s: the connector must never mutate GitLab", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	op, fixture, ok := route(r)
	if !ok {
		s.t.Errorf("unexpected path %q", r.URL.EscapedPath())
		http.NotFound(w, r)
		return
	}
	s.observe(op, r)
	if s.hook != nil && s.hook(r, w) {
		return
	}

	// Only the commit history is paged in the fixtures; one page ends every other walk.
	next := ""
	if op == "commits" && r.URL.Query().Get("page") == "1" {
		next = "2"
	}
	w.Header().Set("X-Next-Page", next)
	s.writeFixture(w, fixture)
}

func (s *stub) observe(op string, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[op]++
	s.token = r.Header.Get("PRIVATE-TOKEN")
	s.queries[op] = append(s.queries[op], r.URL.RawQuery)
}

func (s *stub) wait(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waited = append(s.waited, d)
	return nil
}

func (s *stub) writeFixture(w http.ResponseWriter, name string) {
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		s.t.Errorf("read fixture %s: %v", name, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(body); err != nil {
		s.t.Errorf("write fixture %s: %v", name, err)
	}
}

func (s *stub) callCount(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

func (s *stub) tokenHeader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

func (s *stub) sentQueries(op string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.queries[op])
}

func (s *stub) allQueries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []string
	for _, queries := range s.queries {
		all = append(all, queries...)
	}
	return all
}

func (s *stub) waits() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.waited)
}

type stream struct {
	batches  []lore.Batch
	err      error
	afterErr int // yields observed after the first error: must stay 0
}

// drain consumes the whole iterator without breaking, so a yield after an error
// would be observed rather than hidden by an early return.
func drain(t *testing.T, c *Connector, cursor lore.Cursor) stream {
	t.Helper()
	var got stream
	for batch, err := range c.Changes(context.Background(), cursor) {
		if got.err != nil {
			got.afterErr++
			continue
		}
		if err != nil {
			got.err = err
			continue
		}
		got.batches = append(got.batches, batch)
	}
	if got.afterErr != 0 {
		t.Errorf("iterator yielded %d times after an error", got.afterErr)
	}
	return got
}

func batchedIDs(batches []lore.Batch) [][]lore.DocID {
	out := make([][]lore.DocID, 0, len(batches))
	for _, b := range batches {
		ids := make([]lore.DocID, 0, len(b.Docs))
		for _, d := range b.Docs {
			ids = append(ids, d.ID)
		}
		out = append(out, ids)
	}
	return out
}

func sameIDs(a, b [][]lore.DocID) bool {
	return slices.EqualFunc(a, b, func(x, y []lore.DocID) bool { return slices.Equal(x, y) })
}

func allDocs(batches []lore.Batch) []lore.Document {
	var docs []lore.Document
	for _, b := range batches {
		docs = append(docs, b.Docs...)
	}
	return docs
}

func docsByID(t *testing.T, batches []lore.Batch) map[lore.DocID]lore.Document {
	t.Helper()
	docs := make(map[lore.DocID]lore.Document)
	for _, d := range allDocs(batches) {
		if _, ok := docs[d.ID]; ok {
			t.Errorf("%s yielded twice", d.ID)
		}
		docs[d.ID] = d
	}
	return docs
}

func TestConformance(t *testing.T) {
	s := newStub(t)
	docs := 0
	for _, batch := range wantBatchedIDs() {
		docs += len(batch)
	}
	conform.Run(t, func() lore.Connector { return s.connector() }, conform.Fixture{
		Docs:             docs,
		ResumeAfterBatch: 1,
		ReplayableTypes:  []lore.DocType{lore.DocTypeCommit},
	})
}

func TestChangesStreamsOldestFirstInUnitBatches(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}

	if ids := batchedIDs(got.batches); !sameIDs(ids, wantBatchedIDs()) {
		t.Fatalf("batches\n got %v\nwant %v", ids, wantBatchedIDs())
	}

	// Commits, merge requests and issues interleave by their own watermark. A note
	// keeps its own (older) edit time and travels with the parent that surfaced it.
	tops := map[lore.DocType]bool{
		lore.DocTypeCommit: true, lore.DocTypePR: true, lore.DocTypeIssue: true,
	}
	var last lore.Document
	for _, d := range allDocs(got.batches) {
		if !tops[d.Type] {
			continue
		}
		if !last.UpdatedAt.IsZero() && d.UpdatedAt.Before(last.UpdatedAt) {
			t.Errorf("%s updated %s follows %s updated %s: stream is not oldest-first",
				d.ID, d.UpdatedAt, last.ID, last.UpdatedAt)
		}
		last = d
	}
}

func TestEveryBatchCarriesAnAdvancedCursor(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}

	want := wantCursors()
	if len(got.batches) != len(want) {
		t.Fatalf("got %d batches, want %d", len(got.batches), len(want))
	}
	for i, b := range got.batches {
		if !maps.Equal(b.Cursor, want[i]) {
			t.Errorf("batch %d cursor\n got %v\nwant %v", i, b.Cursor, want[i])
		}
	}
}

// The commit history spans two fixture pages; only X-Next-Page says so.
func TestCommitHistoryFollowsThePageHeader(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}

	if n := s.callCount("commits"); n != 2 {
		t.Errorf("commits called %d times, want 2 pages", n)
	}
	for i, query := range s.sentQueries("commits") {
		if want := "per_page=100"; !strings.Contains(query, want) {
			t.Errorf("commits page %d query %q lacks %q", i+1, query, want)
		}
	}
	docs := docsByID(t, got.batches)
	for _, id := range []lore.DocID{commit1ID, commit2ID, commit3ID} {
		if _, ok := docs[id]; !ok {
			t.Errorf("%s is missing: the second page was not read", id)
		}
	}
}

func TestTokenTravelsOnlyInThePrivateTokenHeader(t *testing.T) {
	s := newStub(t)
	if got := drain(t, s.connector(), nil); got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	if got := s.tokenHeader(); got != fakeToken {
		t.Errorf("PRIVATE-TOKEN = %q, want %q", got, fakeToken)
	}
	for _, query := range s.allQueries() {
		if strings.Contains(query, fakeToken) {
			t.Errorf("query %q carries the token", query)
		}
	}
}

// Only the watermarked collections take a server-side filter; the sub-collections
// of a changed parent are always read whole.
func TestWatermarkIsSentAsAServerSideFilter(t *testing.T) {
	s := newStub(t)
	if got := drain(t, s.connector(), nil); got.err != nil {
		t.Fatalf("first pass: %v", got.err)
	}
	for _, op := range []string{"commits", "merge requests", "issues"} {
		for _, query := range s.sentQueries(op) {
			if strings.Contains(query, "updated_after") || strings.Contains(query, "since=") {
				t.Errorf("%s query %q filters on a watermark during a full backfill", op, query)
			}
		}
	}

	resumed := newStub(t)
	cursor := lore.Cursor{
		projectPath + ":updated_at": "2024-05-02T15:00:00Z",
		projectPath + ":doc_id":     string(issue12ID),
	}
	if got := drain(t, resumed.connector(), cursor); got.err != nil {
		t.Fatalf("resumed pass: %v", got.err)
	}
	tests := map[string]string{
		"commits":        "since=2024-05-02T15%3A00%3A00Z",
		"merge requests": "updated_after=2024-05-02T15%3A00%3A00Z",
		"issues":         "updated_after=2024-05-02T15%3A00%3A00Z",
	}
	for op, want := range tests {
		queries := resumed.sentQueries(op)
		if len(queries) == 0 {
			t.Errorf("%s was never called", op)
			continue
		}
		if !strings.Contains(queries[0], want) {
			t.Errorf("%s query %q lacks %q", op, queries[0], want)
		}
	}
	for _, op := range []string{"merge requests", "issues"} {
		if query := resumed.sentQueries(op)[0]; !strings.Contains(query, "order_by=updated_at&") ||
			!strings.Contains(query, "sort=asc") {
			t.Errorf("%s query %q must ask for an ascending updated_at order", op, query)
		}
	}
}

func TestChangesResumesFromCursor(t *testing.T) {
	s := newStub(t)
	first := drain(t, s.connector(), nil)
	if first.err != nil {
		t.Fatalf("first pass: %v", first.err)
	}

	// Resuming re-reads the overlap window — the stub, like GitLab, has no idea what
	// was committed — and must yield only what the cursor has not covered.
	resumed := drain(t, s.connector(), first.batches[1].Cursor)
	if resumed.err != nil {
		t.Fatalf("resumed pass: %v", resumed.err)
	}
	want := [][]lore.DocID{{mr8ID, single8ID}, {commit3ID}}
	if ids := batchedIDs(resumed.batches); !sameIDs(ids, want) {
		t.Fatalf("resumed batches\n got %v\nwant %v", ids, want)
	}

	final := first.batches[len(first.batches)-1].Cursor
	exhausted := drain(t, s.connector(), final)
	if exhausted.err != nil {
		t.Fatalf("exhausted pass: %v", exhausted.err)
	}
	// The last cursor sits on a commit's own second, which replays by design.
	if ids := batchedIDs(exhausted.batches); !sameIDs(ids, [][]lore.DocID{{commit3ID}}) {
		t.Errorf("resuming from the final cursor yielded %v, want the watermark commit alone", ids)
	}
}

// A cursor can land on an item's own second with a document id that sorts above
// it. An immutable commit has to be replayed there — nothing would ever bring it
// back — while a merge request or issue is skipped and returns on its next edit.
func TestEqualSecondWatermarkReplaysCommitsOnly(t *testing.T) {
	tests := []struct {
		name      string
		updatedAt string
		want      [][]lore.DocID
	}{
		{
			name:      "a commit on the watermark second is replayed",
			updatedAt: "2024-05-02T10:00:00Z",
			want: [][]lore.DocID{
				{commit2ID, issue12ID, note12ID},
				{mr8ID, single8ID},
				{commit3ID},
			},
		},
		{
			name:      "a merge request on the watermark second is skipped",
			updatedAt: "2024-05-03T12:00:00Z",
			want:      [][]lore.DocID{{commit3ID}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStub(t)
			cursor := lore.Cursor{
				projectPath + ":updated_at": tt.updatedAt,
				// Sorts above every fixture document id, so the tiebreak alone decides.
				projectPath + ":doc_id": "gitlab:zzzz",
			}
			got := drain(t, s.connector(), cursor)
			if got.err != nil {
				t.Fatalf("Changes: %v", got.err)
			}
			if ids := batchedIDs(got.batches); !sameIDs(ids, tt.want) {
				t.Fatalf("batches\n got %v\nwant %v", ids, tt.want)
			}
		})
	}
}

func TestDocumentMetadata(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	docs := docsByID(t, got.batches)

	// The issue fixture carries no web_url, so its documents fall back to a URL
	// built from the configured instance root.
	issueURL := s.server.URL + "/" + projectPath + "/-/issues/12"

	tests := []struct {
		id      lore.DocID
		typ     lore.DocType
		title   string
		author  string
		url     string
		created string
		updated string
	}{
		{
			id: commit2ID, typ: lore.DocTypeCommit,
			title: "Cap the auth retry loop at five attempts", author: "Dana Lin",
			url:     fixtureWeb + "/-/commit/" + sha2,
			created: "2024-05-02T09:55:00Z", updated: "2024-05-02T10:00:00Z",
		},
		{
			id: mr7ID, typ: lore.DocTypePR,
			title: "Cap auth retries", author: "dana",
			url:     fixtureWeb + "/-/merge_requests/7",
			created: "2024-05-01T08:00:00Z", updated: "2024-05-01T09:30:00Z",
		},
		{
			id: review7ID, typ: lore.DocTypePRReview,
			title: "Review thread (resolved) on acme/widgets!7", author: "sam",
			url:     fixtureWeb + "/-/merge_requests/7#note_1001",
			created: "2024-05-01T09:00:00Z", updated: "2024-05-01T09:05:00Z",
		},
		{
			id: reply7ID, typ: lore.DocTypeReviewComment,
			title: "Review comment on acme/widgets!7", author: "dana",
			url:     fixtureWeb + "/-/merge_requests/7#note_1002",
			created: "2024-05-01T09:20:00Z", updated: "2024-05-01T09:20:00Z",
		},
		{
			id: single8ID, typ: lore.DocTypeReviewComment,
			title: "Review comment on acme/widgets!8", author: "sam",
			url:     fixtureWeb + "/-/merge_requests/8#note_1010",
			created: "2024-05-03T11:45:00Z", updated: "2024-05-03T11:45:00Z",
		},
		{
			id: issue12ID, typ: lore.DocTypeIssue,
			title: "Auth retries hammer the provider", author: "sam", url: issueURL,
			created: "2024-04-28T10:00:00Z", updated: "2024-05-02T15:00:00Z",
		},
		{
			id: note12ID, typ: lore.DocTypeIssueComment,
			title: "Comment on acme/widgets#12", author: "ravi", url: issueURL + "#note_2001",
			created: "2024-05-01T10:00:00Z", updated: "2024-05-01T10:00:00Z",
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.id), func(t *testing.T) {
			d, ok := docs[tt.id]
			if !ok {
				t.Fatalf("%s was not yielded", tt.id)
			}
			if d.Source != sourceName {
				t.Errorf("Source = %q, want %q", d.Source, sourceName)
			}
			if d.RepoRef != repoRefAll {
				t.Errorf("RepoRef = %q, want %q", d.RepoRef, repoRefAll)
			}
			if d.Type != tt.typ {
				t.Errorf("Type = %q, want %q", d.Type, tt.typ)
			}
			if d.Title != tt.title {
				t.Errorf("Title = %q, want %q", d.Title, tt.title)
			}
			if d.Author != tt.author {
				t.Errorf("Author = %q, want %q", d.Author, tt.author)
			}
			if d.URL != tt.url {
				t.Errorf("URL = %q, want %q", d.URL, tt.url)
			}
			if got := d.CreatedAt.UTC().Format(time.RFC3339); got != tt.created {
				t.Errorf("CreatedAt = %s, want %s", got, tt.created)
			}
			if got := d.UpdatedAt.UTC().Format(time.RFC3339); got != tt.updated {
				t.Errorf("UpdatedAt = %s, want %s", got, tt.updated)
			}
		})
	}

	if body := docs[mr7ID].Body; !strings.HasPrefix(body, "Fixes #12.") {
		t.Errorf("merge request body = %q, want the description verbatim", body)
	}
	if body := docs[commit2ID].Body; !strings.Contains(body, "The unbounded loop hammered") {
		t.Errorf("commit body = %q, want the full message rather than the headline", body)
	}
}

// The chunker derives a Chunk's thread by cutting a note's external id at "#",
// so that prefix has to be exactly the parent's external id.
func TestNoteIDsStripToTheirThread(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}

	parents := map[lore.DocID]string{
		review7ID: projectPath + "/pull/7",
		reply7ID:  projectPath + "/pull/7",
		single7ID: projectPath + "/pull/7",
		single8ID: projectPath + "/pull/8",
		note12ID:  projectPath + "/issues/12",
	}
	for id, want := range parents {
		external := strings.TrimPrefix(string(id), sourceName+":")
		_, external, _ = strings.Cut(external, ":")
		thread, _, ok := strings.Cut(external, "#")
		if !ok {
			t.Errorf("%s carries no note fragment", id)
			continue
		}
		if thread != want {
			t.Errorf("%s threads to %q, want %q", id, thread, want)
		}
		if strings.Count(external, "#") != 1 {
			t.Errorf("%s has %d fragments, want exactly one", id, strings.Count(external, "#"))
		}
	}
}

func TestSystemNotesAreNotIndexed(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	for _, d := range allDocs(got.batches) {
		for _, system := range []string{"note_1004", "note_2002"} {
			if strings.Contains(string(d.ID), system) {
				t.Errorf("%s is a system note and must not be indexed", d.ID)
			}
		}
	}
}

func TestReferenceExtraction(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	docs := docsByID(t, got.batches)

	tests := []struct {
		id   lore.DocID
		want []lore.RawRef
	}{
		{
			// Diff paths first, then whatever the message names.
			id: commit1ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindFilePath, Value: "internal/auth/auth.go"},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#12"},
			},
		},
		{
			// A rename contributes both of its paths.
			id: commit2ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindFilePath, Value: "internal/auth/auth.go"},
				{Kind: lore.RefKindFilePath, Value: "internal/auth/auth_client.go"},
				{Kind: lore.RefKindFilePath, Value: "internal/auth/old_auth.go"},
				{Kind: lore.RefKindTicketKey, Value: "PROJ-123"},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#7"},
			},
		},
		{
			id: commit3ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindFilePath, Value: "docs/auth.md"},
				{Kind: lore.RefKindCommitSHA, Value: "9f8e7d6"},
			},
		},
		{
			// Every commit on the branch, the merge commit, then the prose.
			id: mr7ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindCommitSHA, Value: sha1},
				{Kind: lore.RefKindCommitSHA, Value: sha2},
				{Kind: lore.RefKindCommitSHA, Value: sha3},
				{Kind: lore.RefKindTicketKey, Value: "PROJ-123"},
				{Kind: lore.RefKindURL, Value: "https://www.notion.so/acme/Auth-spec-abc123"},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#12"},
			},
		},
		{
			// A diff note is anchored to its file and to the merge request it argues about.
			id: review7ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindFilePath, Value: "internal/auth/auth.go"},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#7"},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#12"},
			},
		},
		{
			id: reply7ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#7"},
				{Kind: lore.RefKindCommitSHA, Value: "9f8e7d6"},
			},
		},
		{
			id: single7ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#7"},
				{Kind: lore.RefKindTicketKey, Value: "PROJ-123"},
			},
		},
		{
			// "!7" and "#7" name different things in GitLab prose, and the same pair of
			// candidates to the resolver, so both qualify to the project path.
			id: mr8ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindCommitSHA, Value: sha3},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#7"},
			},
		},
		{
			id:   issue12ID,
			want: []lore.RawRef{{Kind: lore.RefKindTicketKey, Value: "PROJ-123"}},
		},
		{
			id: note12ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#12"},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#7"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.id), func(t *testing.T) {
			d, ok := docs[tt.id]
			if !ok {
				t.Fatalf("%s was not yielded", tt.id)
			}
			if !slices.Equal(d.Refs, tt.want) {
				t.Errorf("Refs\n got %v\nwant %v", d.Refs, tt.want)
			}
		})
	}
}

func TestMalformedCursorIsRejectedWithoutFetching(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), lore.Cursor{projectPath + ":updated_at": "last tuesday"})
	if got.err == nil {
		t.Fatal("Changes accepted a malformed watermark")
	}
	if len(got.batches) != 0 {
		t.Errorf("got %d batches, want none", len(got.batches))
	}
	if n := s.callCount("commits"); n != 0 {
		t.Errorf("commits called %d times, want 0", n)
	}
}

func TestInvalidProjectIsRejectedWithoutFetching(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connectorFor([]string{"widgets"}), nil)
	if got.err == nil {
		t.Fatal("Changes accepted a project with no namespace")
	}
	if !strings.Contains(got.err.Error(), `want "group/project"`) {
		t.Errorf("error %q should say what a project looks like", got.err)
	}
	if n := s.callCount("commits"); n != 0 {
		t.Errorf("commits called %d times, want 0", n)
	}
}

func TestCursorIsCopiedPerBatch(t *testing.T) {
	s := newStub(t)
	caller := lore.Cursor{
		projectPath + ":updated_at": "2024-05-01T09:30:00Z",
		projectPath + ":doc_id":     string(mr7ID),
	}
	got := drain(t, s.connector(), caller)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	if len(got.batches) < 2 {
		t.Fatalf("got %d batches, want at least 2", len(got.batches))
	}
	if maps.Equal(got.batches[0].Cursor, got.batches[1].Cursor) {
		t.Error("consecutive batches share one cursor map")
	}
	if caller[projectPath+":doc_id"] != string(mr7ID) {
		t.Error("Changes mutated the caller's cursor")
	}
}

func TestRetriesThrottledRequest(t *testing.T) {
	s := newStub(t)
	s.hook = func(r *http.Request, w http.ResponseWriter) bool {
		if op, _, _ := route(r); op != "commits" || s.callCount("commits") != 1 {
			return false
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"429 Too Many Requests"}`))
		return true
	}

	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes after a 429: %v", got.err)
	}
	if ids := batchedIDs(got.batches); !sameIDs(ids, wantBatchedIDs()) {
		t.Fatalf("batches\n got %v\nwant %v", ids, wantBatchedIDs())
	}
	// The throttled attempt plus both history pages.
	if n := s.callCount("commits"); n != 3 {
		t.Errorf("commits called %d times, want 3 (one 429 retried, then two pages)", n)
	}
	if waited := s.waits(); !slices.Equal(waited, []time.Duration{time.Second}) {
		t.Errorf("waited %v, want one Retry-After second", waited)
	}
}

func TestServerErrorsRetryWithExponentialBackoff(t *testing.T) {
	s := newStub(t)
	s.hook = func(r *http.Request, w http.ResponseWriter) bool {
		if op, _, _ := route(r); op != "commit diff" {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"503 Service Unavailable"}`))
		return true
	}

	got := drain(t, s.connector(), nil)
	if got.err == nil {
		t.Fatal("Changes succeeded, want the diff fetch failure")
	}
	if len(got.batches) != 0 {
		t.Errorf("got %d batches, want none", len(got.batches))
	}
	// History is newest-first, so the newest commit is the one that fails first.
	for _, want := range []string{"gitlab " + projectPath, sha3, "503", "3 attempts"} {
		if !strings.Contains(got.err.Error(), want) {
			t.Errorf("error %q should mention %q", got.err, want)
		}
	}
	if waited := s.waits(); !slices.Equal(waited, []time.Duration{time.Millisecond, 2 * time.Millisecond}) {
		t.Errorf("waited %v, want a doubling backoff", waited)
	}
}

func TestPermanentErrorFailsFastWithoutLeakingTheToken(t *testing.T) {
	s := newStub(t)
	s.hook = func(r *http.Request, w http.ResponseWriter) bool {
		if op, _, _ := route(r); op != "merge requests" {
			return false
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
		return true
	}

	got := drain(t, s.connector(), nil)
	if got.err == nil {
		t.Fatal("Changes succeeded, want the authentication error")
	}
	if n := s.callCount("merge requests"); n != 1 {
		t.Errorf("merge requests called %d times, want 1: a 401 must not be retried", n)
	}
	msg := got.err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("error %q must name the status", msg)
	}
	if strings.Contains(msg, fakeToken) {
		t.Errorf("error %q leaks the token", msg)
	}
}

func TestName(t *testing.T) {
	if got := NewConnector(fakeToken, nil, "").Name(); got != sourceName {
		t.Errorf("Name() = %q, want %q", got, sourceName)
	}
}

func TestNewConnectorResolvesTheInstanceRoot(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		wantAPI string
		wantWeb string
	}{
		{name: "empty means gitlab.com", wantAPI: DefaultBaseURL + "/api/v4", wantWeb: DefaultBaseURL},
		{
			name: "self-managed root", baseURL: "https://gitlab.acme.dev",
			wantAPI: "https://gitlab.acme.dev/api/v4", wantWeb: "https://gitlab.acme.dev",
		},
		{
			name: "a trailing slash is trimmed", baseURL: "https://gitlab.acme.dev/",
			wantAPI: "https://gitlab.acme.dev/api/v4", wantWeb: "https://gitlab.acme.dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewConnector(fakeToken, []string{projectPath}, tt.baseURL)
			if c.client.apiBase != tt.wantAPI {
				t.Errorf("apiBase = %q, want %q", c.client.apiBase, tt.wantAPI)
			}
			if c.webRoot != tt.wantWeb {
				t.Errorf("webRoot = %q, want %q", c.webRoot, tt.wantWeb)
			}
		})
	}
}

func TestParseProject(t *testing.T) {
	tests := []struct {
		in          string
		wantPath    string
		wantEncoded string
		wantErr     bool
	}{
		{in: "acme/widgets", wantPath: "acme/widgets", wantEncoded: "acme%2Fwidgets"},
		{
			in: "acme/platform/infra", wantPath: "acme/platform/infra",
			wantEncoded: "acme%2Fplatform%2Finfra",
		},
		{in: "/acme/widgets/", wantPath: "acme/widgets", wantEncoded: "acme%2Fwidgets"},
		{in: "widgets", wantErr: true},
		{in: "", wantErr: true},
		{in: "acme//widgets", wantErr: true},
		{in: "acme/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseProject(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseProject(%q) = %+v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProject(%q): %v", tt.in, err)
			}
			if got.path != tt.wantPath || got.encoded != tt.wantEncoded {
				t.Errorf("parseProject(%q) = %q/%q, want %q/%q",
					tt.in, got.path, got.encoded, tt.wantPath, tt.wantEncoded)
			}
		})
	}
}

func TestNextPage(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		page   int
		got    int
		want   int
	}{
		{
			name:   "the header advertises the next page",
			header: http.Header{"X-Next-Page": {"4"}}, page: 3, got: pageSize, want: 4,
		},
		{name: "an empty header ends the walk", header: http.Header{"X-Next-Page": {""}}, page: 3, got: pageSize},
		{name: "a stale header never walks backwards", header: http.Header{"X-Next-Page": {"2"}}, page: 3, got: pageSize},
		{name: "no header and a full page keeps going", header: http.Header{}, page: 1, got: pageSize, want: 2},
		{name: "no header and a short page ends the walk", header: http.Header{}, page: 1, got: 7},
		{name: "no header and an empty page ends the walk", header: http.Header{}, page: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextPage(tt.header, tt.page, tt.got); got != tt.want {
				t.Errorf("nextPage = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRetryableStatus(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{status: http.StatusTooManyRequests, want: true},
		{status: http.StatusInternalServerError, want: true},
		{status: http.StatusBadGateway, want: true},
		{status: http.StatusServiceUnavailable, want: true},
		{status: http.StatusGatewayTimeout, want: true},
		{status: http.StatusUnauthorized},
		{status: http.StatusForbidden},
		{status: http.StatusNotFound},
		{status: http.StatusBadRequest},
	}
	for _, tt := range tests {
		if got := retryableStatus(tt.status); got != tt.want {
			t.Errorf("retryableStatus(%d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestRetryDelayTrustsOnlyRetryAfter(t *testing.T) {
	now := time.Date(2024, 5, 5, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{name: "no guidance", header: http.Header{}},
		{name: "seconds", header: http.Header{"Retry-After": {"30"}}, want: 30 * time.Second},
		{
			name:   "http date",
			header: http.Header{"Retry-After": {now.Add(45 * time.Second).Format(http.TimeFormat)}},
			want:   45 * time.Second,
		},
		{name: "negative seconds", header: http.Header{"Retry-After": {"-5"}}, want: -5 * time.Second},
		{name: "a reset epoch is not a delay", header: http.Header{"Ratelimit-Reset": {"1714910400"}}},
		{name: "unparseable", header: http.Header{"Retry-After": {"soon"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryDelay(tt.header, now); got != tt.want {
				t.Errorf("retryDelay = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestOversizedRetryAfterEndsTheRound(t *testing.T) {
	s := newStub(t)
	s.hook = func(r *http.Request, w http.ResponseWriter) bool {
		if op, _, _ := route(r); op != "commits" {
			return false
		}
		w.Header().Set("Retry-After", "100000")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	}

	got := drain(t, s.connector(), nil)
	if got.err == nil {
		t.Fatal("Changes accepted a retry delay beyond the cap")
	}
	for _, want := range []string{"exceeds", maxRetryWait.String(), "429"} {
		if !strings.Contains(got.err.Error(), want) {
			t.Errorf("error %q should mention %q", got.err, want)
		}
	}
	if n := s.callCount("commits"); n != 1 {
		t.Errorf("commits called %d times, want 1", n)
	}
	if waited := s.waits(); len(waited) != 0 {
		t.Errorf("waited %v, want no sleep at all", waited)
	}
}

func TestBackoffGrowsAndSaturates(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: time.Second},
		{attempt: 2, want: 2 * time.Second},
		{attempt: 3, want: 4 * time.Second},
		{attempt: 6, want: maxBackoff},
		{attempt: 99, want: maxBackoff},
	}
	for _, tt := range tests {
		if got := backoff(time.Second, tt.attempt); got != tt.want {
			t.Errorf("backoff(attempt %d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
}

func TestAPIMessageReadsBothFailureShapes(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "routing failure", payload: `{"message":"404 Project Not Found"}`, want: "404 Project Not Found"},
		{name: "scope failure", payload: `{"error":"insufficient_scope"}`, want: "insufficient_scope"},
		{
			name:    "field errors stay verbatim",
			payload: `{"message":{"base":["is invalid"]}}`,
			want:    `{"base":["is invalid"]}`,
		},
		{name: "an html error page", payload: "<html>502</html>", want: "<html>502</html>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := apiMessage([]byte(tt.payload)); got != tt.want {
				t.Errorf("apiMessage = %q, want %q", got, tt.want)
			}
		})
	}
}

// A token passed as a query parameter is a documented GitLab alternative, so an
// error that echoes a URL must not carry one.
func TestRedactScrubsTokenParameters(t *testing.T) {
	got := redact("https://gitlab.acme.dev/api/v4/projects/x?private_token=" + fakeToken + "&page=2")
	if strings.Contains(got, fakeToken) {
		t.Errorf("redact left the token in %q", got)
	}
	if !strings.Contains(got, "page=2") {
		t.Errorf("redact dropped a harmless parameter: %q", got)
	}
}
