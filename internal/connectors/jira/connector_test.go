package jira

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/connectors/conformance"
	"github.com/setthasit/Lore/internal/entities"
)

const (
	// Obviously not credentials; the stub asserts they reach the Authorization
	// header and nowhere else.
	fakeEmail = "bot@example.invalid"
	fakeToken = "fake-api-token"

	wantBasicAuth = "Basic Ym90QGV4YW1wbGUuaW52YWxpZDpmYWtlLWFwaS10b2tlbg=="

	searchPath    = "/rest/api/3/search/jql"
	issuePrefix   = "/rest/api/3/issue/"
	commentSuffix = "/comment"
)

// Documents the fixtures are expected to produce, in stream order.
const (
	proj101ID   entities.DocID = "jira:ticket:PROJ-101"
	proj101c1ID entities.DocID = "jira:ticket_comment:PROJ-101#10001"
	proj101c2ID entities.DocID = "jira:ticket_comment:PROJ-101#10002"
	proj101c3ID entities.DocID = "jira:ticket_comment:PROJ-101#10003"
	proj102ID   entities.DocID = "jira:ticket:PROJ-102"
	proj123ID   entities.DocID = "jira:ticket:PROJ-123"
	proj123c1ID entities.DocID = "jira:ticket_comment:PROJ-123#10010"
	infra7ID    entities.DocID = "jira:ticket:INFRA-7"
)

func fixtureProjects() []string { return []string{"PROJ", "INFRA"} }

func wantBatchedIDs() [][]entities.DocID {
	return [][]entities.DocID{
		{proj101ID, proj101c1ID, proj101c2ID, proj101c3ID},
		{proj102ID, proj123ID, proj123c1ID},
		{infra7ID},
	}
}

func wantCursors() []entities.Cursor {
	return []entities.Cursor{
		{"updated_at": "2024-05-01T09:30:00Z", "doc_id": string(proj101ID)},
		{"updated_at": "2024-05-03T12:00:00.5Z", "doc_id": string(proj123ID)},
		{"updated_at": "2024-05-04T08:00:00Z", "doc_id": string(infra7ID)},
	}
}

// stub replays hand-written fixtures shaped like real Jira Cloud responses. hook,
// when set, may answer a request itself — that is how failure modes are injected.
type stub struct {
	t      *testing.T
	server *httptest.Server
	hook   func(r *http.Request, w http.ResponseWriter) bool

	mu     sync.Mutex
	calls  map[string]int
	auth   string
	jqls   []string
	waited []time.Duration
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{t: t, calls: make(map[string]int)}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stub) connector(opts ...Option) *Connector {
	return s.connectorFor(fixtureProjects(), opts...)
}

// Batches close after two documents so boundaries are observable; retries record
// their wait instead of taking it.
func (s *stub) connectorFor(projects []string, opts ...Option) *Connector {
	base := []Option{withBatchSize(2), withMaxAttempts(3), withBackoff(time.Millisecond)}
	c := NewConnector(s.server.URL, fakeEmail, fakeToken, projects, append(base, opts...)...)
	c.client.sleep = s.wait
	return c
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.t.Errorf("%s %s: the connector must never mutate Jira", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.observe(r)
	if s.hook != nil && s.hook(r, w) {
		return
	}

	if r.URL.Path == searchPath {
		if token := r.URL.Query().Get("nextPageToken"); token == "" {
			s.writeFixture(w, "search_page1.json")
		} else if token == "CAEaAmVu" {
			s.writeFixture(w, "search_page2.json")
		} else {
			s.t.Errorf("unexpected nextPageToken %q", token)
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	key, ok := issueKeyOf(r.URL.Path)
	if !ok {
		s.t.Errorf("unexpected path %q", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	s.writeFixture(w, "comments_"+key+"_"+r.URL.Query().Get("startAt")+".json")
}

func (s *stub) observe(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth = r.Header.Get("Authorization")
	if r.URL.Path == searchPath {
		s.calls["search"]++
		s.jqls = append(s.jqls, r.URL.Query().Get("jql"))
		return
	}
	key, _ := issueKeyOf(r.URL.Path)
	s.calls["comments "+key]++
}

func (s *stub) wait(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waited = append(s.waited, d)
	return nil
}

func issueKeyOf(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, issuePrefix)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(rest, commentSuffix)
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

func (s *stub) authHeader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth
}

func (s *stub) sentJQL() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.jqls)
}

func (s *stub) waits() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.waited)
}

type stream struct {
	batches  []entities.Batch
	err      error
	afterErr int // yields observed after the first error: must stay 0
}

// drain consumes the whole iterator without breaking, so a yield after an error
// would be observed rather than hidden by an early return.
func drain(t *testing.T, c *Connector, cursor entities.Cursor) stream {
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

func batchedIDs(batches []entities.Batch) [][]entities.DocID {
	out := make([][]entities.DocID, 0, len(batches))
	for _, b := range batches {
		ids := make([]entities.DocID, 0, len(b.Docs))
		for _, d := range b.Docs {
			ids = append(ids, d.ID)
		}
		out = append(out, ids)
	}
	return out
}

func sameIDs(a, b [][]entities.DocID) bool {
	return slices.EqualFunc(a, b, func(x, y []entities.DocID) bool { return slices.Equal(x, y) })
}

func allDocs(batches []entities.Batch) []entities.Document {
	var docs []entities.Document
	for _, b := range batches {
		docs = append(docs, b.Docs...)
	}
	return docs
}

func docsByID(t *testing.T, batches []entities.Batch) map[entities.DocID]entities.Document {
	t.Helper()
	docs := make(map[entities.DocID]entities.Document)
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
	conformance.Run(t, func() entities.Connector { return s.connector() }, conformance.Fixture{
		Docs:             docs,
		ResumeAfterBatch: 1,
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

	// Oldest-first orders the issues. A comment keeps its own (older) edit time and
	// travels with the issue whose update surfaced it.
	var last entities.Document
	for _, d := range allDocs(got.batches) {
		if d.Type != entities.DocTypeTicket {
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

func TestAuthorizationIsBasicFromEmailAndToken(t *testing.T) {
	s := newStub(t)
	if got := drain(t, s.connector(), nil); got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	if got := s.authHeader(); got != wantBasicAuth {
		t.Errorf("Authorization = %q, want %q", got, wantBasicAuth)
	}
}

func TestJQLTemplate(t *testing.T) {
	watermark := time.Date(2024, 5, 3, 12, 0, 0, int(500*time.Millisecond), time.UTC)
	tests := []struct {
		name      string
		projects  []string
		watermark time.Time
		want      string
	}{
		{
			name:     "a first run filters on nothing but the projects",
			projects: fixtureProjects(),
			want:     "project IN (PROJ, INFRA) ORDER BY updated ASC",
		},
		{
			name:      "a resume backs the literal off by a day",
			projects:  fixtureProjects(),
			watermark: watermark,
			want:      `project IN (PROJ, INFRA) AND updated >= "2024-05-02 12:00" ORDER BY updated ASC`,
		},
		{
			name:      "no projects means the whole site",
			watermark: watermark,
			want:      `updated >= "2024-05-02 12:00" ORDER BY updated ASC`,
		},
		{
			name: "no projects and no watermark",
			want: "ORDER BY updated ASC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewConnector("https://acme.atlassian.net", fakeEmail, fakeToken, tt.projects)
			if got := c.jql(tt.watermark); got != tt.want {
				t.Errorf("jql\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestJQLOnTheWire(t *testing.T) {
	s := newStub(t)
	first := drain(t, s.connector(), nil)
	if first.err != nil {
		t.Fatalf("Changes: %v", first.err)
	}
	for _, jql := range s.sentJQL() {
		if jql != "project IN (PROJ, INFRA) ORDER BY updated ASC" {
			t.Errorf("first run sent %q, want no updated filter", jql)
		}
	}

	resumed := drain(t, s.connector(), first.batches[0].Cursor)
	if resumed.err != nil {
		t.Fatalf("resumed Changes: %v", resumed.err)
	}
	// The cursor watermark is 2024-05-01T09:30:00Z; the literal is minute-granular,
	// zone-free and a day behind it.
	want := `project IN (PROJ, INFRA) AND updated >= "2024-04-30 09:30" ORDER BY updated ASC`
	for _, jql := range s.sentJQL()[2:] {
		if jql != want {
			t.Errorf("resumed run sent\n got %s\nwant %s", jql, want)
		}
	}
}

func TestUnsafeProjectKeyIsRejectedWithoutFetching(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "quote", key: `PROJ"`},
		{name: "clause escape", key: `X) OR updated >= "1970-01-01 00:00" --`},
		{name: "space", key: "MY PROJ"},
		{name: "lowercase", key: "proj"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStub(t)
			got := drain(t, s.connectorFor([]string{"PROJ", tt.key}), nil)
			if got.err == nil {
				t.Fatalf("Changes accepted project key %q", tt.key)
			}
			if !strings.Contains(got.err.Error(), strconv.Quote(tt.key)) {
				t.Errorf("error %q must name the offending key %q", got.err, tt.key)
			}
			if len(got.batches) != 0 {
				t.Errorf("got %d batches, want none", len(got.batches))
			}
			if n := s.callCount("search"); n != 0 {
				t.Errorf("search called %d times, want 0", n)
			}
		})
	}
}

func TestValidProjectKeysReachTheQueryVerbatim(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connectorFor([]string{"PROJ", "INFRA2", "AB_C"}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	want := "project IN (PROJ, INFRA2, AB_C) ORDER BY updated ASC"
	for _, jql := range s.sentJQL() {
		if jql != want {
			t.Errorf("sent\n got %s\nwant %s", jql, want)
		}
	}
}

func TestDocumentMetadata(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	docs := docsByID(t, got.batches)

	tests := []struct {
		id        entities.DocID
		docType   entities.DocType
		title     string
		author    string
		path      string
		body      string
		createdAt string
		updatedAt string
	}{
		{
			id: proj101ID, docType: entities.DocTypeTicket,
			title:  "PROJ-101: Auth timeout on cold start",
			author: "Ada Lovelace",
			path:   "/browse/PROJ-101",
			body: "Retries hammer the gateway; see [the rollout note](https://www.notion.so/acme/Rollout-Note-4f21) and INFRA-7 for the queue side.\n" +
				"Tracked under PROJ-101; the fix lands in internal/auth/auth.go.",
			createdAt: "2024-04-28T08:05:00Z", updatedAt: "2024-05-01T09:30:00Z",
		},
		{
			id: proj101c1ID, docType: entities.DocTypeTicketComment,
			title:  "Comment on PROJ-101",
			author: "Grace Hopper",
			path:   "/browse/PROJ-101?focusedCommentId=10001",
			body:   "Confirmed with the gateway team; INFRA-7 covers the queue.",
			// A comment keeps its own edit time, older than the issue's watermark.
			createdAt: "2024-04-29T09:00:00Z", updatedAt: "2024-04-29T09:00:00Z",
		},
		{
			// No updated field: the created time fills it.
			id: proj101c2ID, docType: entities.DocTypeTicketComment,
			title:     "Comment on PROJ-101",
			author:    "Alan Turing",
			path:      "/browse/PROJ-101?focusedCommentId=10002",
			body:      "Same root cause as PROJ-101 above.",
			createdAt: "2024-04-30T14:20:00Z", updatedAt: "2024-04-30T14:20:00Z",
		},
		{
			id: proj101c3ID, docType: entities.DocTypeTicketComment,
			title:     "Comment on PROJ-101",
			author:    "Ada Lovelace",
			path:      "/browse/PROJ-101?focusedCommentId=10003",
			body:      "Shipped; the backoff now lives in internal/auth/retry.go",
			createdAt: "2024-05-01T09:30:00Z", updatedAt: "2024-05-01T09:30:00Z",
		},
		{
			// A null description is an empty body, never an error.
			id: proj102ID, docType: entities.DocTypeTicket,
			title:     "PROJ-102: Document the retry budget",
			author:    "Grace Hopper",
			path:      "/browse/PROJ-102",
			body:      "",
			createdAt: "2024-04-30T10:00:00Z", updatedAt: "2024-05-02T11:15:00Z",
		},
		{
			id: proj123ID, docType: entities.DocTypeTicket,
			title:  "PROJ-123: Decide the queue backpressure policy",
			author: "Alan Turing",
			path:   "/browse/PROJ-123",
			body: "## Options\n- Drop on overflow\n- Block the producer, as PROJ-101 asked\n" +
				"https://www.notion.so/acme/Backpressure-9c4d",
			createdAt: "2024-05-02T13:20:00Z", updatedAt: "2024-05-03T12:00:00.5Z",
		},
		{
			id: proj123c1ID, docType: entities.DocTypeTicketComment,
			title:     "Comment on PROJ-123",
			author:    "Katherine Johnson",
			path:      "/browse/PROJ-123?focusedCommentId=10010",
			body:      "Going with blocking; INFRA-7 gets the dead letter queue.",
			createdAt: "2024-05-03T12:00:00.5Z", updatedAt: "2024-05-03T12:00:00.5Z",
		},
		{
			// No reporter: an unassigned author is empty, not an error.
			id: infra7ID, docType: entities.DocTypeTicket,
			title:     "INFRA-7: Provision the retry queue",
			author:    "",
			path:      "/browse/INFRA-7",
			body:      "",
			createdAt: "2024-05-04T07:00:00Z", updatedAt: "2024-05-04T08:00:00Z",
		},
	}

	if len(tests) != len(docs) {
		t.Fatalf("the stream carries %d documents, the table describes %d", len(docs), len(tests))
	}
	for _, tt := range tests {
		d, ok := docs[tt.id]
		if !ok {
			t.Errorf("%s missing from the stream", tt.id)
			continue
		}
		if d.Source != sourceName || d.Type != tt.docType {
			t.Errorf("%s: source %q type %q, want %q %q", tt.id, d.Source, d.Type, sourceName, tt.docType)
		}
		if d.RepoRef != "" {
			t.Errorf("%s: repo ref %q, want empty: Jira is not a repository", tt.id, d.RepoRef)
		}
		if d.Title != tt.title {
			t.Errorf("%s: title %q, want %q", tt.id, d.Title, tt.title)
		}
		if d.Author != tt.author {
			t.Errorf("%s: author %q, want %q", tt.id, d.Author, tt.author)
		}
		if want := s.server.URL + tt.path; d.URL != want {
			t.Errorf("%s: url %q, want %q", tt.id, d.URL, want)
		}
		if d.Body != tt.body {
			t.Errorf("%s: body\n got %q\nwant %q", tt.id, d.Body, tt.body)
		}
		if got := d.CreatedAt.UTC().Format(time.RFC3339Nano); got != tt.createdAt {
			t.Errorf("%s: created %s, want %s", tt.id, got, tt.createdAt)
		}
		if got := d.UpdatedAt.UTC().Format(time.RFC3339Nano); got != tt.updatedAt {
			t.Errorf("%s: updated %s, want %s", tt.id, got, tt.updatedAt)
		}
	}
}

func TestRefsQualifyEveryDocument(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	docs := docsByID(t, got.batches)

	tests := []struct {
		id   entities.DocID
		want []entities.RawRef
	}{
		{
			// PROJ-101 names itself in its own description: a self-edge is dropped,
			// the cross-project key survives.
			id: proj101ID,
			want: []entities.RawRef{
				{Kind: entities.RefKindTicketKey, Value: "INFRA-7"},
				{Kind: entities.RefKindURL, Value: "https://www.notion.so/acme/Rollout-Note-4f21"},
				{Kind: entities.RefKindFilePath, Value: "internal/auth/auth.go"},
			},
		},
		{
			id:   proj102ID,
			want: nil,
		},
		{
			id: proj123ID,
			want: []entities.RawRef{
				{Kind: entities.RefKindTicketKey, Value: "PROJ-101"},
				{Kind: entities.RefKindURL, Value: "https://www.notion.so/acme/Backpressure-9c4d"},
			},
		},
		{
			// The parent issue comes first, before anything the text matched.
			id: proj101c1ID,
			want: []entities.RawRef{
				{Kind: entities.RefKindTicketKey, Value: "PROJ-101"},
				{Kind: entities.RefKindTicketKey, Value: "INFRA-7"},
			},
		},
		{
			// The body repeats the parent key; the explicit relation already covers it.
			id: proj101c2ID,
			want: []entities.RawRef{
				{Kind: entities.RefKindTicketKey, Value: "PROJ-101"},
			},
		},
		{
			id: proj101c3ID,
			want: []entities.RawRef{
				{Kind: entities.RefKindTicketKey, Value: "PROJ-101"},
				{Kind: entities.RefKindFilePath, Value: "internal/auth/retry.go"},
			},
		},
		{
			id: proj123c1ID,
			want: []entities.RawRef{
				{Kind: entities.RefKindTicketKey, Value: "PROJ-123"},
				{Kind: entities.RefKindTicketKey, Value: "INFRA-7"},
			},
		},
		{
			id:   infra7ID,
			want: nil,
		},
	}

	for _, tt := range tests {
		if got := docs[tt.id].Refs; !slices.Equal(got, tt.want) {
			t.Errorf("%s refs\n got %v\nwant %v", tt.id, got, tt.want)
		}
	}
}

func TestChangesResumesFromCursor(t *testing.T) {
	s := newStub(t)
	first := drain(t, s.connector(), nil)
	if first.err != nil {
		t.Fatalf("first pass: %v", first.err)
	}

	// Resuming re-fetches the whole overlap window — the stub, like Jira, has no
	// idea what was committed — and must yield only what the cursor has not covered.
	resumed := drain(t, s.connector(), first.batches[0].Cursor)
	if resumed.err != nil {
		t.Fatalf("resumed pass: %v", resumed.err)
	}
	want := [][]entities.DocID{{proj102ID, proj123ID, proj123c1ID}, {infra7ID}}
	if ids := batchedIDs(resumed.batches); !sameIDs(ids, want) {
		t.Fatalf("resumed batches\n got %v\nwant %v", ids, want)
	}

	final := first.batches[len(first.batches)-1].Cursor
	exhausted := drain(t, s.connector(), final)
	if exhausted.err != nil {
		t.Fatalf("exhausted pass: %v", exhausted.err)
	}
	if len(exhausted.batches) != 0 {
		t.Errorf("resuming from the final cursor yielded %d batches, want none: %v",
			len(exhausted.batches), batchedIDs(exhausted.batches))
	}
}

func TestCursorTiebreakSkipsTheWatermarkUnitOnly(t *testing.T) {
	s := newStub(t)
	// PROJ-123's own second with a document id sorting above it: the tie is broken
	// lexicographically, so PROJ-123 stays covered.
	cursor := entities.Cursor{"updated_at": "2024-05-03T12:00:00.5Z", "doc_id": "jira:ticket:ZZZZ-1"}
	got := drain(t, s.connector(), cursor)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	want := [][]entities.DocID{{infra7ID}}
	if ids := batchedIDs(got.batches); !sameIDs(ids, want) {
		t.Fatalf("batches\n got %v\nwant %v", ids, want)
	}
}

func TestMalformedCursorIsRejectedWithoutFetching(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(), entities.Cursor{"updated_at": "last tuesday"})
	if got.err == nil {
		t.Fatal("Changes accepted a malformed watermark")
	}
	if len(got.batches) != 0 {
		t.Errorf("got %d batches, want none", len(got.batches))
	}
	if n := s.callCount("search"); n != 0 {
		t.Errorf("search called %d times, want 0", n)
	}
}

func TestCursorIsCopiedPerBatch(t *testing.T) {
	s := newStub(t)
	caller := entities.Cursor{"updated_at": "2024-05-01T09:30:00Z", "doc_id": string(proj101ID)}
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
	if caller["doc_id"] != string(proj101ID) {
		t.Error("Changes mutated the caller's cursor")
	}
}

func TestRetriesThrottledSearch(t *testing.T) {
	s := newStub(t)
	s.hook = func(r *http.Request, w http.ResponseWriter) bool {
		if r.URL.Path != searchPath || s.callCount("search") != 1 {
			return false
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errorMessages":["Rate limit exceeded"],"errors":{}}`))
		return true
	}

	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes after a 429: %v", got.err)
	}
	if ids := batchedIDs(got.batches); !sameIDs(ids, wantBatchedIDs()) {
		t.Fatalf("batches\n got %v\nwant %v", ids, wantBatchedIDs())
	}
	// The throttled attempt plus both search pages.
	if n := s.callCount("search"); n != 3 {
		t.Errorf("search called %d times, want 3 (one 429 retried, then two pages)", n)
	}
	if waited := s.waits(); !slices.Equal(waited, []time.Duration{time.Second}) {
		t.Errorf("waited %v, want one Retry-After second", waited)
	}
}

func TestServerErrorsRetryWithExponentialBackoff(t *testing.T) {
	s := newStub(t)
	s.hook = func(r *http.Request, w http.ResponseWriter) bool {
		if _, ok := issueKeyOf(r.URL.Path); !ok {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"errorMessages":["Service temporarily unavailable"]}`))
		return true
	}

	got := drain(t, s.connector(), nil)
	if got.err == nil {
		t.Fatal("Changes succeeded, want the comment fetch failure")
	}
	if len(got.batches) != 0 {
		t.Errorf("got %d batches, want none", len(got.batches))
	}
	for _, want := range []string{"PROJ-101", "503", "3 attempts"} {
		if !strings.Contains(got.err.Error(), want) {
			t.Errorf("error %q should mention %q", got.err, want)
		}
	}
	if waited := s.waits(); !slices.Equal(waited, []time.Duration{time.Millisecond, 2 * time.Millisecond}) {
		t.Errorf("waited %v, want a doubling backoff", waited)
	}
}

func TestPermanentErrorFailsFastWithoutLeakingCredentials(t *testing.T) {
	s := newStub(t)
	s.hook = func(r *http.Request, w http.ResponseWriter) bool {
		if r.URL.Path != searchPath {
			return false
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errorMessages":["Client must be authenticated to access this resource."]}`))
		return true
	}

	got := drain(t, s.connector(), nil)
	if got.err == nil {
		t.Fatal("Changes succeeded, want the authentication error")
	}
	if n := s.callCount("search"); n != 1 {
		t.Errorf("search called %d times, want 1: a 401 must not be retried", n)
	}
	msg := got.err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("error %q must name the status", msg)
	}
	for _, secret := range []string{fakeToken, fakeEmail, wantBasicAuth} {
		if strings.Contains(msg, secret) {
			t.Errorf("error %q leaks %q", msg, secret)
		}
	}
}

func TestNewConnectorTrimsTheSiteRoot(t *testing.T) {
	c := NewConnector("https://acme.atlassian.net/", fakeEmail, fakeToken, nil)
	if got := c.Name(); got != sourceName {
		t.Errorf("Name() = %q, want %q", got, sourceName)
	}
	if got := c.browseURL("PROJ-1"); got != "https://acme.atlassian.net/browse/PROJ-1" {
		t.Errorf("browseURL = %q", got)
	}
	if got := c.client.baseURL; got != "https://acme.atlassian.net" {
		t.Errorf("client baseURL = %q", got)
	}
}

func TestTimestampParsesJiraOffsets(t *testing.T) {
	tests := []struct {
		raw     string
		want    time.Time
		wantErr bool
	}{
		{raw: `"2021-01-17T12:34:00.000+0000"`, want: time.Date(2021, 1, 17, 12, 34, 0, 0, time.UTC)},
		{raw: `"2021-01-17T12:34:00.000+0530"`, want: time.Date(2021, 1, 17, 7, 4, 0, 0, time.UTC)},
		{raw: `"2021-01-17T12:34:00.500-0800"`, want: time.Date(2021, 1, 17, 20, 34, 0, int(500*time.Millisecond), time.UTC)},
		{raw: `""`},
		{raw: `"2021-01-17T12:34:00Z"`, wantErr: true},
		{raw: `"17/01/2021"`, wantErr: true},
	}
	for _, tt := range tests {
		var ts timestamp
		err := json.Unmarshal([]byte(tt.raw), &ts)
		if tt.wantErr {
			if err == nil {
				t.Errorf("%s parsed as %s, want an error", tt.raw, ts.Time)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.raw, err)
			continue
		}
		if !ts.Equal(tt.want) {
			t.Errorf("%s = %s, want %s", tt.raw, ts.Time, tt.want)
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
		{
			name:   "http date already in the past",
			header: http.Header{"Retry-After": {now.Add(-30 * time.Second).Format(http.TimeFormat)}},
			want:   -30 * time.Second,
		},
		{name: "negative seconds", header: http.Header{"Retry-After": {"-5"}}, want: -5 * time.Second},
		{
			name:   "exhausted quota without a delay",
			header: http.Header{"X-Ratelimit-Remaining": {"0"}},
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

func TestNegativeRetryAfterFallsBackToBackoff(t *testing.T) {
	s := newStub(t)
	s.hook = func(r *http.Request, w http.ResponseWriter) bool {
		if r.URL.Path != searchPath || s.callCount("search") != 1 {
			return false
		}
		w.Header().Set("Retry-After", "-5")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	}

	got := drain(t, s.connector(), nil)
	if got.err != nil {
		t.Fatalf("Changes after a negative Retry-After: %v", got.err)
	}
	if ids := batchedIDs(got.batches); !sameIDs(ids, wantBatchedIDs()) {
		t.Fatalf("batches\n got %v\nwant %v", ids, wantBatchedIDs())
	}
	if waited := s.waits(); !slices.Equal(waited, []time.Duration{time.Millisecond}) {
		t.Errorf("waited %v, want the exponential backoff base", waited)
	}
}

func TestOversizedRetryAfterEndsTheRound(t *testing.T) {
	s := newStub(t)
	s.hook = func(r *http.Request, w http.ResponseWriter) bool {
		if r.URL.Path != searchPath {
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
	if n := s.callCount("search"); n != 1 {
		t.Errorf("search called %d times, want 1", n)
	}
	if waited := s.waits(); len(waited) != 0 {
		t.Errorf("waited %v, want no sleep at all", waited)
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
