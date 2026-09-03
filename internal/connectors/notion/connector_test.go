package notion

import (
	"context"
	"encoding/json"
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

	"github.com/setthasit/Lore/internal/connectors/conformance"
	"github.com/setthasit/Lore/internal/entities"
)

const fakeToken = "secret_test-token"

const (
	rootPageID      = "11111111-1111-4111-8111-111111111111"
	decisionPageID  = "22222222-2222-4222-8222-222222222222"
	trashedPageID   = "33333333-3333-4333-8333-333333333333"
	runbookPageID   = "44444444-4444-4444-8444-444444444444"
	toggleBlockID   = "55555555-5555-4555-8555-555555555555"
	marketingPageID = "66666666-6666-4666-8666-666666666666"
	marketingHomeID = "88888888-8888-4888-8888-888888888888"
	missingPageID   = "deadbeef-0000-4000-8000-000000000000"
	tieHighPageID   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	tieLowPageID    = "77777777-7777-4777-8777-777777777777"
	tieLatePageID   = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"

	duplicateTitle    = "Design Reviews"
	firstDuplicateID  = "aaaa1111-1111-4111-8111-111111111111"
	secondDuplicateID = "aaaa2222-2222-4222-8222-222222222222"
)

const (
	rootID      entities.DocID = "notion:page:" + rootPageID
	decisionID  entities.DocID = "notion:page:" + decisionPageID
	runbookID   entities.DocID = "notion:page:" + runbookPageID
	trashedID   entities.DocID = "notion:page:" + trashedPageID
	marketingID entities.DocID = "notion:page:" + marketingPageID
	tieHighID   entities.DocID = "notion:page:" + tieHighPageID
	tieLowID    entities.DocID = "notion:page:" + tieLowPageID
	tieLateID   entities.DocID = "notion:page:" + tieLatePageID
)

const (
	rootBody = "Index of engineering decisions.\n---"

	decisionBody = "## Decision\n" +
		"Adopted after review of [the rollout ticket](https://acme.atlassian.net/browse/PROJ-123)" +
		" and touches internal/auth/session.go.\n" +
		"- Rotate the signing keys\n" +
		"  - Track progress in PROJ-456\n" +
		"```go\n" +
		"session.Rotate(ctx)\n" +
		"```\n" +
		"Runbooks\n" +
		"Deep Runbook"

	runbookBody = "Runbook for the auth rollout.\n- [x] Verify staging\n- [ ] Verify prod"
)

func wantBatchedIDs() [][]entities.DocID {
	return [][]entities.DocID{{rootID, decisionID}, {runbookID}}
}

func wantCursors() []entities.Cursor {
	return []entities.Cursor{
		{"last_edited_at": "2024-05-02T10:30:00.123Z", "doc_id": string(decisionID)},
		{"last_edited_at": "2024-05-03T08:00:00Z", "doc_id": string(runbookID)},
	}
}

type stub struct {
	t      *testing.T
	server *httptest.Server
	hook   func(method, path string, w http.ResponseWriter) (handled bool)

	mu      sync.Mutex
	calls   map[string]int
	auth    string
	version string
	delays  []time.Duration
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{t: t, calls: make(map[string]int)}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stub) connector(roots []string, opts ...Option) *Connector {
	base := []Option{withBatchSize(2), withMaxAttempts(3), withBackoff(time.Millisecond)}
	c := NewConnector(fakeToken, roots, s.server.URL, append(base, opts...)...)
	c.client.sleep = s.recordSleep
	return c
}

func (s *stub) recordSleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays = append(s.delays, d)
	return ctx.Err()
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.auth = r.Header.Get("Authorization")
	s.version = r.Header.Get("Notion-Version")
	s.mu.Unlock()

	route := r.Method + " " + r.URL.Path
	s.record(route)
	if s.hook != nil && s.hook(r.Method, r.URL.Path, w) {
		return
	}

	switch {
	case route == "POST /v1/search":
		s.serveSearch(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/blocks/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/blocks/")
		if parent, ok := strings.CutSuffix(id, "/children"); ok {
			s.writeFixture(w, "blocks_"+short(parent)+".json")
			return
		}
		s.writeFixture(w, "block_"+short(id)+".json")
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
		s.writeFixture(w, "page_"+short(strings.TrimPrefix(r.URL.Path, "/v1/pages/"))+".json")
	default:
		s.t.Errorf("unexpected request %s", route)
		http.NotFound(w, r)
	}
}

func (s *stub) serveSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query  string `json:"query"`
		Filter struct {
			Property string `json:"property"`
			Value    string `json:"value"`
		} `json:"filter"`
		Sort struct {
			Timestamp string `json:"timestamp"`
			Direction string `json:"direction"`
		} `json:"sort"`
		StartCursor string `json:"start_cursor"`
		PageSize    int    `json:"page_size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.t.Errorf("decode search request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Filter.Property != "object" || req.Filter.Value != "page" {
		s.t.Errorf("search filter = %+v, want object=page", req.Filter)
	}
	if req.Sort.Timestamp != "last_edited_time" || req.Sort.Direction != "ascending" {
		s.t.Errorf("search sort = %+v, want last_edited_time ascending", req.Sort)
	}
	if req.PageSize < 1 || req.PageSize > 100 {
		s.t.Errorf("search page_size = %d, want 1..100", req.PageSize)
	}

	switch {
	case req.Query == duplicateTitle:
		s.writeFixture(w, "search_duplicate.json")
	case req.Query != "":
		s.writeFixture(w, "search_query.json")
	case req.StartCursor == "":
		s.writeFixture(w, "search_page1.json")
	default:
		s.writeFixture(w, "search_page2.json")
	}
}

func short(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
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

func (s *stub) record(route string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[route]++
}

func (s *stub) callCount(route string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[route]
}

func (s *stub) sleeps() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.delays)
}

func (s *stub) headers() (auth, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth, s.version
}

type stream struct {
	batches        []entities.Batch
	err            error
	yieldsAfterErr int
}

func drain(t *testing.T, c *Connector, cursor entities.Cursor) stream {
	t.Helper()
	var got stream
	for batch, err := range c.Changes(context.Background(), cursor) {
		if got.err != nil {
			got.yieldsAfterErr++
			continue
		}
		if err != nil {
			got.err = err
			continue
		}
		got.batches = append(got.batches, batch)
	}
	if got.yieldsAfterErr != 0 {
		t.Errorf("iterator yielded %d times after an error", got.yieldsAfterErr)
	}
	return got
}

func idsOf(docs []entities.Document) []entities.DocID {
	out := make([]entities.DocID, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.ID)
	}
	return out
}

func batchedIDs(batches []entities.Batch) [][]entities.DocID {
	out := make([][]entities.DocID, 0, len(batches))
	for _, b := range batches {
		out = append(out, idsOf(b.Docs))
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

func docsByID(batches []entities.Batch) map[entities.DocID]entities.Document {
	out := make(map[entities.DocID]entities.Document)
	for _, d := range allDocs(batches) {
		out[d.ID] = d
	}
	return out
}

func TestConformance(t *testing.T) {
	s := newStub(t)
	conformance.Run(t, func() entities.Connector { return s.connector([]string{rootPageID}) }, conformance.Fixture{
		Docs:             3,
		ResumeAfterBatch: 0,
	})
}

func TestChangesStreamsInScopePagesOldestFirst(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{rootPageID}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	if diff := batchedIDs(got.batches); !sameIDs(diff, wantBatchedIDs()) {
		t.Fatalf("batches\n got %v\nwant %v", diff, wantBatchedIDs())
	}

	var last time.Time
	for _, d := range allDocs(got.batches) {
		if d.UpdatedAt.Before(last) {
			t.Errorf("%s updated %s follows %s: stream is not oldest-first", d.ID, d.UpdatedAt, last)
		}
		last = d.UpdatedAt
	}
}

func TestTrashedAndOutOfScopePagesAreSkipped(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{rootPageID}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}

	docs := docsByID(got.batches)
	for _, id := range []entities.DocID{trashedID, marketingID} {
		if _, ok := docs[id]; ok {
			t.Errorf("%s reached the stream", id)
		}
	}
	if _, ok := docs[runbookID]; !ok {
		t.Errorf("%s is a deep descendant of the root and must be indexed", runbookID)
	}

	if n := s.callCount("GET /v1/blocks/" + marketingPageID + "/children"); n != 0 {
		t.Errorf("fetched the body of an out-of-scope page %d times", n)
	}
	if n := s.callCount("GET /v1/blocks/" + toggleBlockID); n != 1 {
		t.Errorf("block ancestry lookups = %d, want 1", n)
	}
	// The runbook climbs one block into a page already decided, so the memo answers it.
	if n := s.callCount("GET /v1/pages/" + decisionPageID); n != 0 {
		t.Errorf("re-resolved a memoised ancestor %d times", n)
	}
	if n := s.callCount("GET /v1/pages/" + marketingHomeID); n != 1 {
		t.Errorf("page ancestry lookups = %d, want 1", n)
	}
}

func TestEmptyRootPagesIndexesEveryVisiblePage(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector(nil), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	want := [][]entities.DocID{{rootID, decisionID}, {runbookID, marketingID}}
	if diff := batchedIDs(got.batches); !sameIDs(diff, want) {
		t.Fatalf("batches\n got %v\nwant %v", diff, want)
	}
	if n := s.callCount("GET /v1/pages/" + marketingHomeID); n != 0 {
		t.Errorf("unscoped sync walked ancestry %d times", n)
	}
}

func TestRootPageEntriesResolve(t *testing.T) {
	tests := []struct {
		name  string
		entry string
	}{
		{name: "dashed page id", entry: rootPageID},
		{name: "undashed page id", entry: strings.ReplaceAll(rootPageID, "-", "")},
		{name: "exact page title", entry: "Engineering Wiki"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStub(t)
			got := drain(t, s.connector([]string{tt.entry}), nil)
			if got.err != nil {
				t.Fatalf("Changes: %v", got.err)
			}
			if diff := batchedIDs(got.batches); !sameIDs(diff, wantBatchedIDs()) {
				t.Fatalf("batches\n got %v\nwant %v", diff, wantBatchedIDs())
			}
		})
	}
}

func TestUnresolvableRootPageIsAnError(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		wantMsg string
	}{
		{name: "no page carries the title", entry: "Nonexistent Space", wantMsg: "Nonexistent Space"},
		{name: "blank entry", entry: "  ", wantMsg: "blank root page entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStub(t)
			got := drain(t, s.connector([]string{tt.entry}), nil)
			if got.err == nil {
				t.Fatal("Changes accepted an unresolvable root page")
			}
			if len(got.batches) != 0 {
				t.Errorf("got %d batches, want none", len(got.batches))
			}
			if !strings.Contains(got.err.Error(), tt.wantMsg) {
				t.Errorf("error %q should mention %q", got.err, tt.wantMsg)
			}
		})
	}
}

func TestDuplicateRootPageTitleIsAnError(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{duplicateTitle}), nil)
	if got.err == nil {
		t.Fatal("Changes silently scoped to one of two live pages sharing the configured title")
	}
	if len(got.batches) != 0 {
		t.Errorf("got %d batches, want none", len(got.batches))
	}
	for _, want := range []string{duplicateTitle, firstDuplicateID, secondDuplicateID} {
		if !strings.Contains(got.err.Error(), want) {
			t.Errorf("error %q should mention %q", got.err, want)
		}
	}
}

func TestRootPageIDIsConfirmedOncePerRoot(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{rootPageID}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	if n := s.callCount("GET /v1/pages/" + normalizeID(rootPageID)); n != 1 {
		t.Errorf("confirmed the root page %d times, want 1", n)
	}
}

func TestUnreadableRootPageIDIsAnError(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		wantMsg string
	}{
		{name: "no such page", entry: missingPageID, wantMsg: "reads back as no page"},
		{name: "trashed page", entry: trashedPageID, wantMsg: "in the trash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStub(t)
			s.hook = func(method, path string, w http.ResponseWriter) bool {
				if method+" "+path != "GET /v1/pages/"+normalizeID(missingPageID) {
					return false
				}
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"object":"error","code":"object_not_found","message":"Could not find page."}`))
				return true
			}

			got := drain(t, s.connector([]string{tt.entry}), nil)
			if got.err == nil {
				t.Fatal("Changes accepted a root page id the workspace cannot serve")
			}
			if len(got.batches) != 0 {
				t.Errorf("got %d batches, want none", len(got.batches))
			}
			for _, want := range []string{tt.entry, tt.wantMsg} {
				if !strings.Contains(got.err.Error(), want) {
					t.Errorf("error %q should mention %q", got.err, want)
				}
			}
			if n := s.callCount("POST /v1/search"); n != 0 {
				t.Errorf("walked the workspace %d times behind an unusable root", n)
			}
		})
	}
}

func TestEveryBatchCarriesAnAdvancedCursor(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{rootPageID}), nil)
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

func TestChangesResumesFromCursorWithoutReplay(t *testing.T) {
	s := newStub(t)
	conn := s.connector([]string{rootPageID})
	first := drain(t, conn, nil)
	if first.err != nil {
		t.Fatalf("first pass: %v", first.err)
	}

	resumed := drain(t, conn, first.batches[0].Cursor)
	if resumed.err != nil {
		t.Fatalf("resumed pass: %v", resumed.err)
	}
	want := [][]entities.DocID{{runbookID}}
	if diff := batchedIDs(resumed.batches); !sameIDs(diff, want) {
		t.Fatalf("resumed batches\n got %v\nwant %v", diff, want)
	}

	final := first.batches[len(first.batches)-1].Cursor
	exhausted := drain(t, conn, final)
	if exhausted.err != nil {
		t.Fatalf("exhausted pass: %v", exhausted.err)
	}
	if len(exhausted.batches) != 0 {
		t.Errorf("resuming from the final cursor yielded %v", batchedIDs(exhausted.batches))
	}
}

func TestBatchNeverClosesInsideOneEditTimestamp(t *testing.T) {
	s := newStub(t)
	s.hook = func(method, path string, w http.ResponseWriter) bool {
		if method+" "+path != "POST /v1/search" {
			return false
		}
		s.writeFixture(w, "search_tie.json")
		return true
	}

	conn := s.connector([]string{rootPageID}, withBatchSize(1))
	full := drain(t, conn, nil)
	if full.err != nil {
		t.Fatalf("Changes: %v", full.err)
	}
	want := [][]entities.DocID{{tieHighID, tieLowID}, {tieLateID}}
	if diff := batchedIDs(full.batches); !sameIDs(diff, want) {
		t.Fatalf("batches\n got %v\nwant %v", diff, want)
	}

	for i, b := range full.batches {
		resumed := drain(t, conn, b.Cursor)
		if resumed.err != nil {
			t.Fatalf("resume from batch %d: %v", i, resumed.err)
		}
		got, rest := idsOf(allDocs(resumed.batches)), idsOf(allDocs(full.batches[i+1:]))
		if !slices.Equal(got, rest) {
			t.Errorf("resume from the batch %d cursor\n got %v\nwant %v", i, got, rest)
		}
	}
}

func TestMillisecondWatermarkDoesNotReplayTheSameSecond(t *testing.T) {
	s := newStub(t)
	cursor := entities.Cursor{"last_edited_at": "2024-05-02T10:30:00.123Z", "doc_id": string(decisionID)}
	got := drain(t, s.connector([]string{rootPageID}), cursor)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	want := [][]entities.DocID{{runbookID}}
	if diff := batchedIDs(got.batches); !sameIDs(diff, want) {
		t.Fatalf("batches\n got %v\nwant %v", diff, want)
	}
}

func TestDocumentMetadata(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{rootPageID}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	docs := docsByID(got.batches)

	tests := []struct {
		id        entities.DocID
		title     string
		body      string
		url       string
		createdAt string
		updatedAt string
	}{
		{
			id:        rootID,
			title:     "Engineering Wiki",
			body:      rootBody,
			url:       "https://www.notion.so/acme/Engineering-Wiki-1111",
			createdAt: "2024-04-01T08:00:00Z",
			updatedAt: "2024-05-01T09:00:00Z",
		},
		{
			id:        decisionID,
			title:     "Auth Rework Decision",
			body:      decisionBody,
			url:       "https://www.notion.so/acme/Auth-Rework-Decision-2222",
			createdAt: "2024-04-18T13:05:00Z",
			updatedAt: "2024-05-02T10:30:00.123Z",
		},
		{
			id:        runbookID,
			title:     "Deep Runbook",
			body:      runbookBody,
			url:       "https://www.notion.so/acme/Deep-Runbook-4444",
			createdAt: "2024-04-25T09:15:00Z",
			updatedAt: "2024-05-03T08:00:00Z",
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.id), func(t *testing.T) {
			d, ok := docs[tt.id]
			if !ok {
				t.Fatalf("%s missing from the stream", tt.id)
			}
			if d.Source != sourceName || d.Type != entities.DocTypePage {
				t.Errorf("source %q type %q", d.Source, d.Type)
			}
			if d.RepoRef != "" {
				t.Errorf("RepoRef = %q, want empty: a page belongs to no repository", d.RepoRef)
			}
			if d.Author != "" {
				t.Errorf("Author = %q, want empty: the page object carries no display name", d.Author)
			}
			if d.Title != tt.title {
				t.Errorf("title\n got %q\nwant %q", d.Title, tt.title)
			}
			if d.Body != tt.body {
				t.Errorf("body\n got %q\nwant %q", d.Body, tt.body)
			}
			if d.URL != tt.url {
				t.Errorf("url = %q, want %q", d.URL, tt.url)
			}
			if got := d.CreatedAt.UTC().Format(time.RFC3339Nano); got != tt.createdAt {
				t.Errorf("CreatedAt = %s, want %s", got, tt.createdAt)
			}
			if got := d.UpdatedAt.UTC().Format(time.RFC3339Nano); got != tt.updatedAt {
				t.Errorf("UpdatedAt = %s, want %s", got, tt.updatedAt)
			}
		})
	}
}

func TestReferenceExtraction(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{rootPageID}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}

	want := []entities.RawRef{
		{Kind: entities.RefKindTicketKey, Value: "PROJ-123"},
		{Kind: entities.RefKindTicketKey, Value: "PROJ-456"},
		{Kind: entities.RefKindURL, Value: "https://acme.atlassian.net/browse/PROJ-123"},
		{Kind: entities.RefKindFilePath, Value: "internal/auth/session.go"},
	}
	if refs := docsByID(got.batches)[decisionID].Refs; !slices.Equal(refs, want) {
		t.Errorf("refs of %s\n got %v\nwant %v", decisionID, refs, want)
	}
}

func TestMalformedCursorIsRejectedWithoutFetching(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{rootPageID}), entities.Cursor{"last_edited_at": "last tuesday"})
	if got.err == nil {
		t.Fatal("Changes accepted a malformed watermark")
	}
	if len(got.batches) != 0 {
		t.Errorf("got %d batches, want none", len(got.batches))
	}
	if n := s.callCount("POST /v1/search"); n != 0 {
		t.Errorf("searched %d times before rejecting the cursor", n)
	}
}

func TestCursorIsCopiedPerBatch(t *testing.T) {
	s := newStub(t)
	caller := entities.Cursor{"last_edited_at": "2020-01-01T00:00:00Z", "doc_id": "notion:page:older"}
	got := drain(t, s.connector([]string{rootPageID}), caller)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	if len(got.batches) < 2 {
		t.Fatalf("got %d batches, want at least 2", len(got.batches))
	}
	if maps.Equal(got.batches[0].Cursor, got.batches[1].Cursor) {
		t.Error("consecutive batches share one cursor map")
	}
	if caller["doc_id"] != "notion:page:older" {
		t.Error("Changes mutated the caller's cursor")
	}
}

func TestRetriesThrottledAndOverloadedResponses(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		retryAfter string
		wantDelays []time.Duration
	}{
		{
			name:       "429 honours retry-after seconds",
			status:     http.StatusTooManyRequests,
			retryAfter: "1",
			wantDelays: []time.Duration{time.Second},
		},
		{name: "529 service overload backs off", status: 529, wantDelays: []time.Duration{time.Millisecond}},
		{name: "500 backs off", status: http.StatusInternalServerError, wantDelays: []time.Duration{time.Millisecond}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStub(t)
			s.hook = func(method, path string, w http.ResponseWriter) bool {
				if method+" "+path != "POST /v1/search" || s.callCount("POST /v1/search") != 1 {
					return false
				}
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"object":"error","code":"rate_limited","message":"slow down"}`))
				return true
			}

			got := drain(t, s.connector([]string{rootPageID}), nil)
			if got.err != nil {
				t.Fatalf("Changes after a %d: %v", tt.status, got.err)
			}
			if diff := batchedIDs(got.batches); !sameIDs(diff, wantBatchedIDs()) {
				t.Fatalf("batches\n got %v\nwant %v", diff, wantBatchedIDs())
			}
			if n := s.callCount("POST /v1/search"); n != 3 {
				t.Errorf("search called %d times, want 3 (one rejected attempt, then two result pages)", n)
			}
			if delays := s.sleeps(); !slices.Equal(delays, tt.wantDelays) {
				t.Errorf("retry delays = %v, want %v", delays, tt.wantDelays)
			}
		})
	}
}

func TestPermanentStatusFailsFast(t *testing.T) {
	s := newStub(t)
	s.hook = func(method, path string, w http.ResponseWriter) bool {
		if method+" "+path != "POST /v1/search" {
			return false
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"object":"error","code":"unauthorized","message":"API token is invalid."}`))
		return true
	}

	got := drain(t, s.connector([]string{rootPageID}), nil)
	if got.err == nil {
		t.Fatal("Changes succeeded, want the authorization failure")
	}
	if len(got.batches) != 0 {
		t.Errorf("got %d batches, want none", len(got.batches))
	}
	if n := s.callCount("POST /v1/search"); n != 1 {
		t.Errorf("search called %d times, want 1: a 401 must not be retried", n)
	}
	if msg := got.err.Error(); !strings.Contains(msg, "401") || strings.Contains(msg, fakeToken) {
		t.Errorf("error %q must name the status and never the token", msg)
	}
}

func TestExhaustedRetriesFailWithTheStatus(t *testing.T) {
	s := newStub(t)
	s.hook = func(method, path string, w http.ResponseWriter) bool {
		if method+" "+path != "POST /v1/search" {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"object":"error","code":"service_unavailable","message":"try later"}`))
		return true
	}

	got := drain(t, s.connector([]string{rootPageID}, withMaxAttempts(2)), nil)
	if got.err == nil {
		t.Fatal("Changes succeeded, want the exhausted retries")
	}
	for _, want := range []string{"2 attempts", "503"} {
		if !strings.Contains(got.err.Error(), want) {
			t.Errorf("error %q should mention %q", got.err, want)
		}
	}
	if n := s.callCount("POST /v1/search"); n != 2 {
		t.Errorf("search called %d times, want 2", n)
	}
}

func TestOversizedRetryDelayEndsTheRound(t *testing.T) {
	s := newStub(t)
	s.hook = func(method, path string, w http.ResponseWriter) bool {
		if method+" "+path != "POST /v1/search" {
			return false
		}
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"object":"error","code":"rate_limited","message":"slow down"}`))
		return true
	}

	got := drain(t, s.connector([]string{rootPageID}), nil)
	if got.err == nil {
		t.Fatal("Changes accepted an hour-long server-requested delay")
	}
	for _, want := range []string{"1h0m0s", maxRetryWait.String()} {
		if !strings.Contains(got.err.Error(), want) {
			t.Errorf("error %q should mention %q", got.err, want)
		}
	}
	if delays := s.sleeps(); len(delays) != 0 {
		t.Errorf("waited %v instead of ending the round", delays)
	}
	if n := s.callCount("POST /v1/search"); n != 1 {
		t.Errorf("search called %d times, want 1", n)
	}
}

func TestCredentialTravelsOnlyInTheAuthorizationHeader(t *testing.T) {
	s := newStub(t)
	got := drain(t, s.connector([]string{rootPageID}), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}

	auth, version := s.headers()
	if want := "Bearer " + fakeToken; auth != want {
		t.Errorf("authorization header = %q, want %q", auth, want)
	}
	if version != apiVersion {
		t.Errorf("Notion-Version = %q, want %q", version, apiVersion)
	}
	for _, d := range allDocs(got.batches) {
		if strings.Contains(d.Body, fakeToken) || strings.Contains(d.URL, fakeToken) {
			t.Errorf("%s echoes the token", d.ID)
		}
	}
}

func TestName(t *testing.T) {
	if got := NewConnector(fakeToken, nil, "").Name(); got != sourceName {
		t.Errorf("Name() = %q, want %q", got, sourceName)
	}
}

func TestPageTrashed(t *testing.T) {
	tests := []struct {
		name string
		page page
		want bool
	}{
		{name: "live"},
		{name: "in trash", page: page{InTrash: true}, want: true},
		{name: "archived alias", page: page{Archived: true}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.page.trashed(); got != tt.want {
				t.Errorf("trashed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPageTitleFindsThePropertyByType(t *testing.T) {
	tests := []struct {
		name string
		page page
		want string
	}{
		{
			name: "property named something else",
			page: page{Properties: map[string]property{
				"Owner": {Type: "people"},
				"Name":  {Type: "title", Title: []richText{{PlainText: "Auth "}, {PlainText: "Rework"}}},
			}},
			want: "Auth Rework",
		},
		{
			name: "no title property",
			page: page{Properties: map[string]property{"Owner": {Type: "people"}}},
			want: untitled,
		},
		{
			name: "empty title text",
			page: page{Properties: map[string]property{"title": {Type: "title"}}},
			want: untitled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.page.title(); got != tt.want {
				t.Errorf("title() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsPageID(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: rootPageID, want: true},
		{in: strings.ToUpper(rootPageID), want: true},
		{in: strings.ReplaceAll(rootPageID, "-", ""), want: true},
		{in: "Engineering Wiki"},
		{in: "11111111-1111-4111-8111-11111111111"},
		{in: "gggggggg-1111-4111-8111-111111111111"},
		{in: ""},
	}
	for _, tt := range tests {
		if got := isPageID(normalizeID(tt.in)); got != tt.want {
			t.Errorf("isPageID(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{name: "no guidance", header: http.Header{}},
		{name: "seconds", header: http.Header{"Retry-After": {"30"}}, want: 30 * time.Second},
		{name: "zero", header: http.Header{"Retry-After": {"0"}}},
		{name: "unparseable", header: http.Header{"Retry-After": {"soon"}}},
		{name: "http date is never sent", header: http.Header{"Retry-After": {"Wed, 21 Oct 2026 07:28:00 GMT"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryDelay(tt.header); got != tt.want {
				t.Errorf("retryDelay = %s, want %s", got, tt.want)
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
		{status: 529, want: true},
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
