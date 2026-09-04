package github

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
)

const (
	// fakeToken is obviously not a credential; the stub asserts it arrives in
	// the Authorization header and nowhere else.
	fakeToken   = "ghp_fake_test_token"
	fixtureRepo = "acme/widgets"

	shaA = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	shaB = "1a2b3c4d5e6f708192a3b4c5d6e7f80910111213"
)

// Documents the fixtures are expected to produce, in stream order.
const (
	commitBID   lore.DocID = "github:commit:acme/widgets/commit/" + shaB
	commitAID   lore.DocID = "github:commit:acme/widgets/commit/" + shaA
	pr42ID      lore.DocID = "github:pr:acme/widgets/pull/42"
	review1ID   lore.DocID = "github:pr_review:acme/widgets/pull/42#pullrequestreview-8801"
	comment1ID  lore.DocID = "github:review_comment:acme/widgets/pull/42#discussion_r9901"
	comment2ID  lore.DocID = "github:review_comment:acme/widgets/pull/42#discussion_r9902"
	review2ID   lore.DocID = "github:pr_review:acme/widgets/pull/42#pullrequestreview-8802"
	issue41ID   lore.DocID = "github:issue:acme/widgets/issues/41"
	icomment1ID lore.DocID = "github:issue_comment:acme/widgets/issues/41#issuecomment-7701"
	icomment2ID lore.DocID = "github:issue_comment:acme/widgets/issues/41#issuecomment-7702"
)

func wantBatchedIDs() [][]lore.DocID {
	return [][]lore.DocID{
		{commitBID, commitAID},
		{pr42ID, review1ID, comment1ID, comment2ID, review2ID},
		{issue41ID, icomment1ID, icomment2ID},
	}
}

func wantCursors() []lore.Cursor {
	return []lore.Cursor{
		{"acme/widgets:updated_at": "2024-05-03T12:00:00Z", "acme/widgets:doc_id": string(commitAID)},
		{"acme/widgets:updated_at": "2024-05-04T15:00:00Z", "acme/widgets:doc_id": string(pr42ID)},
		{"acme/widgets:updated_at": "2024-05-05T10:00:00Z", "acme/widgets:doc_id": string(issue41ID)},
	}
}

var operationPattern = regexp.MustCompile(`query\s+(\w+)`)

// stub replays hand-written fixtures shaped like real GraphQL and REST
// responses. hook, when set, may answer a request itself — that is how failure
// modes are injected.
type stub struct {
	t      *testing.T
	server *httptest.Server
	hook   func(op string, vars map[string]any, w http.ResponseWriter) bool

	mu    sync.Mutex
	calls map[string]int
	auth  string
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{t: t, calls: make(map[string]int)}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

// connector builds a connector against the stub. Batches close after two
// documents so batch boundaries are observable, and retries sleep for a
// millisecond instead of a second.
func (s *stub) connector(repos []string, opts ...Option) *Connector {
	base := []Option{withBatchSize(2), withMaxAttempts(3), withBackoff(time.Millisecond)}
	return NewConnector(fakeToken, repos, s.server.URL, append(base, opts...)...)
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.auth = r.Header.Get("Authorization")
	s.mu.Unlock()

	if r.URL.Path != "/graphql" {
		s.serveREST(w, r)
		return
	}

	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.t.Errorf("decode graphql request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	match := operationPattern.FindStringSubmatch(req.Query)
	if match == nil {
		s.t.Errorf("graphql query carries no operation name: %s", req.Query)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	op := match[1]
	s.record(op)

	if s.hook != nil && s.hook(op, req.Variables, w) {
		return
	}
	name, ok := fixtureFor(op, req.Variables)
	if !ok {
		s.t.Errorf("no fixture for operation %q", op)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// LoreReviewComments addresses a review by node id and carries no repository.
	if got, ok := req.Variables["name"]; ok && got != "widgets" {
		s.t.Errorf("%s: unexpected repository name %v", op, got)
	}
	s.writeFixture(w, name)
}

func (s *stub) serveREST(w http.ResponseWriter, r *http.Request) {
	sha, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/commits/")
	if !ok || len(sha) < 7 {
		s.t.Errorf("unexpected REST path %q", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	s.record("REST commit")
	s.writeFixture(w, "rest_commit_"+sha[:7]+".json")
}

func fixtureFor(op string, vars map[string]any) (string, bool) {
	switch op {
	case "LoreCommits":
		if vars["after"] == nil {
			return "commits_page1.json", true
		}
		return "commits_page2.json", true
	case "LorePullRequests":
		return "prs_page1.json", true
	case "LoreReviews":
		return "pr_reviews_page2.json", true
	case "LoreReviewComments":
		return "review_comments_page2.json", true
	case "LorePRCommits":
		return "pr_commits_page2.json", true
	case "LoreIssues":
		return "issues_page1.json", true
	case "LoreIssueComments":
		return "issue_comments_page2.json", true
	}
	return "", false
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

func (s *stub) record(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[op]++
}

func (s *stub) callCount(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

func (s *stub) authHeader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth
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

func TestConformance(t *testing.T) {
	s := newStub(t)
	docs := 0
	for _, batch := range wantBatchedIDs() {
		docs += len(batch)
	}
	conform.Run(t, func() lore.Connector { return s.connector([]string{fixtureRepo}) }, conform.Fixture{
		Docs: docs,
		// A commit whose committed date ties with the cursor's second is
		// replayed rather than risked; nothing else re-enters the stream.
		ReplayableTypes: []lore.DocType{lore.DocTypeCommit},
	})
}

func TestChangesStreamsOldestFirstInBatches(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{fixtureRepo}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}

	if diff := batchedIDs(got.batches); !sameIDs(diff, wantBatchedIDs()) {
		t.Fatalf("batches\n got %v\nwant %v", diff, wantBatchedIDs())
	}

	// Oldest-first orders the top-level items. A review or comment keeps its own
	// (older) edit time and travels with the parent whose update surfaced it.
	var last lore.Document
	for _, d := range allDocs(got.batches) {
		switch d.Type {
		case lore.DocTypeCommit, lore.DocTypePR, lore.DocTypeIssue:
		default:
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
	got := drain(t, s.connector([]string{fixtureRepo}), nil)
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

func TestChangesResumesFromCursor(t *testing.T) {
	s := newStub(t)
	conn := s.connector([]string{fixtureRepo})
	first := drain(t, conn, nil)
	if first.err != nil {
		t.Fatalf("first pass: %v", first.err)
	}

	// Resuming after the first batch re-fetches everything the API offers — the
	// stub, like GitHub, has no idea what was committed — and must yield only
	// what the cursor has not covered. The watermark commit is the exception: it
	// shares the watermark's second, so it is replayed rather than risked.
	resumed := drain(t, conn, first.batches[0].Cursor)
	if resumed.err != nil {
		t.Fatalf("resumed pass: %v", resumed.err)
	}
	wantResumed := [][]lore.DocID{
		append([]lore.DocID{commitAID}, wantBatchedIDs()[1]...),
		wantBatchedIDs()[2],
	}
	if got := batchedIDs(resumed.batches); !sameIDs(got, wantResumed) {
		t.Fatalf("resumed batches\n got %v\nwant %v", got, wantResumed)
	}

	final := first.batches[len(first.batches)-1].Cursor
	exhausted := drain(t, conn, final)
	if exhausted.err != nil {
		t.Fatalf("exhausted pass: %v", exhausted.err)
	}
	if len(exhausted.batches) != 0 {
		t.Errorf("resuming from the final cursor yielded %d batches, want none: %v",
			len(exhausted.batches), batchedIDs(exhausted.batches))
	}
}

// A cursor can land on an item's own second with a document id that sorts above
// it. An immutable commit has to be replayed there — nothing would ever bring it
// back — while a pull request or issue is skipped and returns on its next edit.
func TestEqualSecondWatermarkReplaysCommitsOnly(t *testing.T) {
	tests := []struct {
		name   string
		cursor lore.Cursor
		want   []lore.DocID
		absent []lore.DocID
	}{
		{
			name: "commit tied with the watermark second is replayed",
			cursor: lore.Cursor{
				"acme/widgets:updated_at": "2024-05-03T12:00:00Z",
				"acme/widgets:doc_id":     "github:pr:acme/widgets/pull/999",
			},
			want:   []lore.DocID{commitAID, pr42ID, issue41ID},
			absent: []lore.DocID{commitBID},
		},
		{
			name: "pull request tied with the watermark second stays skipped",
			cursor: lore.Cursor{
				"acme/widgets:updated_at": "2024-05-04T15:00:00Z",
				"acme/widgets:doc_id":     "github:zzzz",
			},
			want:   []lore.DocID{issue41ID, icomment1ID, icomment2ID},
			absent: []lore.DocID{commitAID, commitBID, pr42ID, review1ID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStub(t)
			got := drain(t, s.connector([]string{fixtureRepo}), tt.cursor)
			if got.err != nil {
				t.Fatalf("Changes: %v", got.err)
			}
			if len(got.batches) == 0 {
				t.Fatal("no batches yielded")
			}
			seen := make(map[lore.DocID]bool)
			for _, d := range allDocs(got.batches) {
				seen[d.ID] = true
			}
			for _, id := range tt.want {
				if !seen[id] {
					t.Errorf("%s missing from the stream", id)
				}
			}
			for _, id := range tt.absent {
				if seen[id] {
					t.Errorf("%s was already covered by the cursor", id)
				}
			}
			// A replayed unit sits below the resume position and must not drag
			// the cursor backwards.
			final := got.batches[len(got.batches)-1].Cursor
			want := wantCursors()[len(wantCursors())-1]
			if !maps.Equal(final, want) {
				t.Errorf("final cursor\n got %v\nwant %v", final, want)
			}
		})
	}
}

func TestEveryDocumentIsFullyIdentified(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{fixtureRepo}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}

	for _, d := range allDocs(got.batches) {
		switch {
		case d.ID == "":
			t.Errorf("document with empty id: %+v", d)
		case d.Source != sourceName:
			t.Errorf("%s: source %q, want %q", d.ID, d.Source, sourceName)
		case d.Type == "":
			t.Errorf("%s: empty type", d.ID)
		case d.RepoRef != "github:acme/widgets":
			t.Errorf("%s: repo ref %q", d.ID, d.RepoRef)
		case d.URL == "":
			t.Errorf("%s: empty url", d.ID)
		case d.Title == "":
			t.Errorf("%s: empty title", d.ID)
		case d.CreatedAt.IsZero():
			t.Errorf("%s: zero CreatedAt", d.ID)
		case d.UpdatedAt.IsZero():
			t.Errorf("%s: zero UpdatedAt", d.ID)
		case d.UpdatedAt.Before(d.CreatedAt):
			t.Errorf("%s: UpdatedAt %s precedes CreatedAt %s", d.ID, d.UpdatedAt, d.CreatedAt)
		}
	}
}

func TestDocumentMetadata(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{fixtureRepo}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	docs := make(map[lore.DocID]lore.Document)
	for _, d := range allDocs(got.batches) {
		docs[d.ID] = d
	}

	tests := []struct {
		id        lore.DocID
		docType   lore.DocType
		title     string
		author    string
		url       string
		createdAt string
		updatedAt string
	}{
		{
			id: commitAID, docType: lore.DocTypeCommit,
			title: "Fix auth timeout on cold start", author: "ada",
			url:       "https://github.com/acme/widgets/commit/" + shaA,
			createdAt: "2024-05-03T11:45:00Z", updatedAt: "2024-05-03T12:00:00Z",
		},
		{
			// No linked GitHub account: the raw git author name is all there is.
			id: commitBID, docType: lore.DocTypeCommit,
			title: "Add widget cache", author: "Grace Hopper",
			url:       "https://github.com/acme/widgets/commit/" + shaB,
			createdAt: "2024-05-01T09:30:00Z", updatedAt: "2024-05-01T09:30:00Z",
		},
		{
			id: pr42ID, docType: lore.DocTypePR,
			title: "Fix auth timeout", author: "ada",
			url:       "https://github.com/acme/widgets/pull/42",
			createdAt: "2024-04-28T08:00:00Z", updatedAt: "2024-05-04T15:00:00Z",
		},
		{
			id: review1ID, docType: lore.DocTypePRReview,
			title: "Review (APPROVED) on acme/widgets#42", author: "grace",
			url:       "https://github.com/acme/widgets/pull/42#pullrequestreview-8801",
			createdAt: "2024-05-02T09:00:00Z", updatedAt: "2024-05-02T09:05:00Z",
		},
		{
			id: review2ID, docType: lore.DocTypePRReview,
			title: "Review (CHANGES_REQUESTED) on acme/widgets#42", author: "ada",
			url:       "https://github.com/acme/widgets/pull/42#pullrequestreview-8802",
			createdAt: "2024-05-03T10:00:00Z", updatedAt: "2024-05-03T10:00:00Z",
		},
		{
			id: comment1ID, docType: lore.DocTypeReviewComment,
			title: "Review comment on acme/widgets#42 (internal/auth/auth.go)", author: "grace",
			url:       "https://github.com/acme/widgets/pull/42#discussion_r9901",
			createdAt: "2024-05-02T09:01:00Z", updatedAt: "2024-05-02T09:01:00Z",
		},
		{
			id: issue41ID, docType: lore.DocTypeIssue,
			title: "Auth times out after deploy", author: "reporter",
			url:       "https://github.com/acme/widgets/issues/41",
			createdAt: "2024-04-20T07:00:00Z", updatedAt: "2024-05-05T10:00:00Z",
		},
		{
			id: icomment2ID, docType: lore.DocTypeIssueComment,
			title: "Comment on acme/widgets#41", author: "grace",
			url:       "https://github.com/acme/widgets/issues/41#issuecomment-7702",
			createdAt: "2024-05-05T09:55:00Z", updatedAt: "2024-05-05T09:55:00Z",
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.id), func(t *testing.T) {
			d, ok := docs[tt.id]
			if !ok {
				t.Fatalf("document missing from stream")
			}
			if d.Type != tt.docType {
				t.Errorf("type = %q, want %q", d.Type, tt.docType)
			}
			if d.Title != tt.title {
				t.Errorf("title = %q, want %q", d.Title, tt.title)
			}
			if d.Author != tt.author {
				t.Errorf("author = %q, want %q", d.Author, tt.author)
			}
			if d.URL != tt.url {
				t.Errorf("url = %q, want %q", d.URL, tt.url)
			}
			if got := d.CreatedAt.UTC().Format(time.RFC3339); got != tt.createdAt {
				t.Errorf("createdAt = %s, want %s", got, tt.createdAt)
			}
			if got := d.UpdatedAt.UTC().Format(time.RFC3339); got != tt.updatedAt {
				t.Errorf("updatedAt = %s, want %s", got, tt.updatedAt)
			}
		})
	}
}

func externalID(t *testing.T, id lore.DocID) string {
	t.Helper()
	parts := strings.SplitN(string(id), ":", 3)
	if len(parts) != 3 {
		t.Fatalf("malformed doc id %q", id)
	}
	return parts[2]
}

// The chunker derives a Chunk's thread by cutting a comment's external id at
// "#", so that prefix has to be exactly the parent's external id.
func TestCommentIDsStripToTheirThread(t *testing.T) {
	tests := []struct {
		comment lore.DocID
		thread  lore.DocID
	}{
		{review1ID, pr42ID},
		{comment1ID, pr42ID},
		{comment2ID, pr42ID},
		{icomment1ID, issue41ID},
		{icomment2ID, issue41ID},
	}
	for _, tt := range tests {
		got, fragment, ok := strings.Cut(externalID(t, tt.comment), "#")
		if !ok || fragment == "" {
			t.Errorf("%s carries no comment fragment", tt.comment)
			continue
		}
		if want := externalID(t, tt.thread); got != want {
			t.Errorf("%s strips to %q, want the thread %q", tt.comment, got, want)
		}
	}
	for _, id := range []lore.DocID{commitAID, pr42ID, issue41ID} {
		if strings.Contains(externalID(t, id), "#") {
			t.Errorf("%s is its own thread and must carry no fragment", id)
		}
	}
}

func TestReferenceExtraction(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{fixtureRepo}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	refs := make(map[lore.DocID][]lore.RawRef)
	for _, d := range allDocs(got.batches) {
		refs[d.ID] = d.Refs
	}

	tests := []struct {
		name string
		id   lore.DocID
		want []lore.RawRef
	}{
		{
			name: "commit: associated pr, touched files including the rename, then text",
			id:   commitAID,
			want: []lore.RawRef{
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#42"},
				{Kind: lore.RefKindFilePath, Value: "internal/auth/auth.go"},
				{Kind: lore.RefKindFilePath, Value: "internal/auth/auth_test.go"},
				{Kind: lore.RefKindFilePath, Value: "internal/auth/old_auth_test.go"},
				{Kind: lore.RefKindTicketKey, Value: "PROJ-123"},
				{Kind: lore.RefKindURL, Value: "https://www.notion.so/acme/Auth-spec-abc123"},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#41"},
				{Kind: lore.RefKindCommitSHA, Value: "1a2b3c4"},
			},
		},
		{
			name: "commit: a ticket key repeated in the message is emitted once",
			id:   commitBID,
			want: []lore.RawRef{
				{Kind: lore.RefKindFilePath, Value: "internal/cache/cache.go"},
				{Kind: lore.RefKindTicketKey, Value: "PROJ-123"},
			},
		},
		{
			name: "pr: closing issue and every commit, branch ticket key, no duplicate #41",
			id:   pr42ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#41"},
				{Kind: lore.RefKindCommitSHA, Value: shaA},
				{Kind: lore.RefKindCommitSHA, Value: shaB},
				{Kind: lore.RefKindTicketKey, Value: "PROJ-123"},
				{Kind: lore.RefKindTicketKey, Value: "PROJ-456"},
				{Kind: lore.RefKindURL, Value: "https://www.notion.so/acme/Auth-spec-abc123"},
			},
		},
		{
			name: "review: parent pull request plus body text",
			id:   review1ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#42"},
				{Kind: lore.RefKindTicketKey, Value: "PROJ-123"},
			},
		},
		{
			name: "review comment: annotated path, parent, cross-reference",
			id:   comment1ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindFilePath, Value: "internal/auth/auth.go"},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#42"},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#41"},
			},
		},
		{
			name: "issue: ticket key, url, qualified cross-reference",
			id:   issue41ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindTicketKey, Value: "PROJ-123"},
				{Kind: lore.RefKindURL, Value: "https://acme.atlassian.net/browse/PROJ-123"},
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#42"},
			},
		},
		{
			name: "issue comment: parent thread and a full sha in the body",
			id:   icomment2ID,
			want: []lore.RawRef{
				{Kind: lore.RefKindPRNumber, Value: "acme/widgets#41"},
				{Kind: lore.RefKindCommitSHA, Value: shaA},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !slices.Equal(refs[tt.id], tt.want) {
				t.Errorf("refs of %s\n got %v\nwant %v", tt.id, refs[tt.id], tt.want)
			}
		})
	}
}

func TestTokenTravelsOnlyInTheAuthorizationHeader(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{fixtureRepo}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	if want := "Bearer " + fakeToken; s.authHeader() != want {
		t.Errorf("authorization header = %q, want %q", s.authHeader(), want)
	}
	for _, d := range allDocs(got.batches) {
		if strings.Contains(d.Body, fakeToken) || strings.Contains(d.URL, fakeToken) {
			t.Errorf("%s echoes the token", d.ID)
		}
	}
}

func TestRetriesSecondaryRateLimit(t *testing.T) {
	s := newStub(t)
	s.hook = func(op string, _ map[string]any, w http.ResponseWriter) bool {
		if op != "LoreCommits" || s.callCount(op) != 1 {
			return false
		}
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`))
		return true
	}

	got := drain(t, s.connector([]string{fixtureRepo}), nil)
	if got.err != nil {
		t.Fatalf("Changes after a 429: %v", got.err)
	}
	if diff := batchedIDs(got.batches); !sameIDs(diff, wantBatchedIDs()) {
		t.Fatalf("batches\n got %v\nwant %v", diff, wantBatchedIDs())
	}
	// The throttled attempt plus both history pages.
	if n := s.callCount("LoreCommits"); n != 3 {
		t.Errorf("LoreCommits called %d times, want 3 (one 429 retried, then two pages)", n)
	}
}

func TestRetriesRateLimitedGraphQLError(t *testing.T) {
	s := newStub(t)
	s.hook = func(op string, _ map[string]any, w http.ResponseWriter) bool {
		if op != "LoreIssues" || s.callCount(op) != 1 {
			return false
		}
		// The primary GraphQL limit arrives as HTTP 200 with no Retry-After.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		return true
	}

	got := drain(t, s.connector([]string{fixtureRepo}), nil)
	if got.err != nil {
		t.Fatalf("Changes after a RATE_LIMITED payload: %v", got.err)
	}
	if len(got.batches) != 3 {
		t.Errorf("got %d batches, want 3", len(got.batches))
	}
	if n := s.callCount("LoreIssues"); n != 2 {
		t.Errorf("LoreIssues called %d times, want 2", n)
	}
}

func TestForbiddenWithoutRateLimitEvidenceFailsFast(t *testing.T) {
	s := newStub(t)
	s.hook = func(op string, _ map[string]any, w http.ResponseWriter) bool {
		if op != "LoreCommits" {
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
		return true
	}

	got := drain(t, s.connector([]string{fixtureRepo}), nil)
	if got.err == nil {
		t.Fatal("Changes succeeded, want a permissions error")
	}
	if len(got.batches) != 0 {
		t.Errorf("got %d batches, want none", len(got.batches))
	}
	if n := s.callCount("LoreCommits"); n != 1 {
		t.Errorf("LoreCommits called %d times, want 1: a bare 403 must not be retried", n)
	}
	if msg := got.err.Error(); !strings.Contains(msg, "403") || strings.Contains(msg, fakeToken) {
		t.Errorf("error %q must name the status and never the token", msg)
	}
}

func TestGraphQLErrorAbortsStream(t *testing.T) {
	s := newStub(t)
	s.hook = func(op string, _ map[string]any, w http.ResponseWriter) bool {
		if op != "LorePullRequests" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository with the name 'acme/widgets'."}]}`))
		return true
	}

	got := drain(t, s.connector([]string{fixtureRepo}), nil)
	if got.err == nil {
		t.Fatal("Changes succeeded, want the GraphQL error")
	}
	if !strings.Contains(got.err.Error(), "NOT_FOUND") {
		t.Errorf("error %q should carry the GraphQL error type", got.err)
	}
	if n := s.callCount("LorePullRequests"); n != 1 {
		t.Errorf("LorePullRequests called %d times, want 1: NOT_FOUND is not retryable", n)
	}
}

func TestHardFailureMidStreamTerminatesIteration(t *testing.T) {
	s := newStub(t)
	s.hook = func(_ string, vars map[string]any, w http.ResponseWriter) bool {
		if vars["name"] != "gadgets" {
			return false
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Server Error"}`))
		return true
	}

	conn := s.connector([]string{fixtureRepo, "acme/gadgets"}, withMaxAttempts(2))
	got := drain(t, conn, nil)
	if got.err == nil {
		t.Fatal("Changes succeeded, want the second repository's failure")
	}
	if diff := batchedIDs(got.batches); !sameIDs(diff, wantBatchedIDs()) {
		t.Fatalf("batches before the failure\n got %v\nwant %v", diff, wantBatchedIDs())
	}
	msg := got.err.Error()
	for _, want := range []string{"acme/gadgets", "500", "2 attempts"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should mention %q", msg, want)
		}
	}
	if strings.Contains(msg, fakeToken) {
		t.Error("error echoes the token")
	}
}

func TestMalformedCursorIsRejectedWithoutFetching(t *testing.T) {
	s := newStub(t)
	cursor := lore.Cursor{"acme/widgets:updated_at": "last tuesday"}
	got := drain(t, s.connector([]string{fixtureRepo}), cursor)
	if got.err == nil {
		t.Fatal("Changes accepted a malformed watermark")
	}
	if len(got.batches) != 0 {
		t.Errorf("got %d batches, want none", len(got.batches))
	}
	if n := s.callCount("LoreCommits"); n != 0 {
		t.Errorf("LoreCommits called %d times, want 0", n)
	}
}

func TestCursorIsCopiedPerBatch(t *testing.T) {
	s := newStub(t)
	caller := lore.Cursor{"acme/widgets:updated_at": "2024-05-03T12:00:00Z", "acme/widgets:doc_id": string(commitAID)}
	got := drain(t, s.connector([]string{fixtureRepo}), caller)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	if len(got.batches) < 2 {
		t.Fatalf("got %d batches, want at least 2", len(got.batches))
	}
	if maps.Equal(got.batches[0].Cursor, got.batches[1].Cursor) {
		t.Error("consecutive batches share one cursor map")
	}
	if caller["acme/widgets:doc_id"] != string(commitAID) {
		t.Error("Changes mutated the caller's cursor")
	}
}

func TestName(t *testing.T) {
	if got := NewConnector(fakeToken, nil, "").Name(); got != "github" {
		t.Errorf("Name() = %q, want %q", got, "github")
	}
}

func TestParseRepo(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{in: "acme/widgets"},
		{in: "acme", wantErr: true},
		{in: "", wantErr: true},
		{in: "/widgets", wantErr: true},
		{in: "acme/", wantErr: true},
		{in: "acme/widgets/extra", wantErr: true},
	}
	for _, tt := range tests {
		r, err := parseRepo(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseRepo(%q) = %+v, want an error", tt.in, r)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRepo(%q): %v", tt.in, err)
			continue
		}
		if r.owner != "acme" || r.name != "widgets" || r.ref() != "github:acme/widgets" {
			t.Errorf("parseRepo(%q) = %+v", tt.in, r)
		}
	}
}

func TestRetryableStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header http.Header
		body   string
		want   bool
	}{
		{name: "429 always", status: http.StatusTooManyRequests, want: true},
		{name: "bare 403", status: http.StatusForbidden, body: `{"message":"Must have push access"}`},
		{
			name: "403 with retry-after", status: http.StatusForbidden,
			header: http.Header{"Retry-After": {"60"}}, want: true,
		},
		{
			name: "403 with exhausted primary limit", status: http.StatusForbidden,
			header: http.Header{"X-Ratelimit-Remaining": {"0"}}, want: true,
		},
		{
			name: "403 naming the secondary limit", status: http.StatusForbidden,
			body: `{"message":"You have exceeded a secondary rate limit"}`, want: true,
		},
		{
			name: "403 naming abuse detection", status: http.StatusForbidden,
			body: `{"message":"You have triggered an abuse detection mechanism"}`, want: true,
		},
		{name: "502 from graphql", status: http.StatusBadGateway, want: true},
		{name: "503", status: http.StatusServiceUnavailable, want: true},
		{name: "504", status: http.StatusGatewayTimeout, want: true},
		{name: "500", status: http.StatusInternalServerError, want: true},
		{name: "401", status: http.StatusUnauthorized},
		{name: "404", status: http.StatusNotFound},
		{name: "422", status: http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := tt.header
			if header == nil {
				header = http.Header{}
			}
			if got := retryableStatus(tt.status, header, []byte(tt.body)); got != tt.want {
				t.Errorf("retryableStatus(%d) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
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
		{
			name: "primary limit reset",
			header: http.Header{
				"X-Ratelimit-Remaining": {"0"},
				"X-Ratelimit-Reset":     {"1714910700"}, // 2024-05-05T12:05:00Z
			},
			want: 5 * time.Minute,
		},
		{
			name:   "quota left means no wait",
			header: http.Header{"X-Ratelimit-Remaining": {"4999"}},
		},
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

func TestBackoffGrowsAndSaturates(t *testing.T) {
	base := time.Second
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: time.Second},
		{attempt: 2, want: 2 * time.Second},
		{attempt: 3, want: 4 * time.Second},
		{attempt: 6, want: maxBackoff}, // 32s would exceed the cap
		{attempt: 99, want: maxBackoff},
	}
	for _, tt := range tests {
		if got := backoff(base, tt.attempt); got != tt.want {
			t.Errorf("backoff(attempt %d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
}

func TestGraphQLURLSplitsEnterpriseRoots(t *testing.T) {
	tests := []struct{ base, want string }{
		{base: "", want: "https://api.github.com/graphql"},
		{base: "https://api.github.com", want: "https://api.github.com/graphql"},
		{base: "https://api.github.com/", want: "https://api.github.com/graphql"},
		{base: "https://git.acme.internal/api/v3", want: "https://git.acme.internal/api/graphql"},
	}
	for _, tt := range tests {
		if got := newClient(fakeToken, tt.base).graphqlURL(); got != tt.want {
			t.Errorf("graphqlURL(%q) = %q, want %q", tt.base, got, tt.want)
		}
	}
}
