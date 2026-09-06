package plugbuild

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/sdk"
)

// roundTripper serves the index from memory: a search test that reached the
// real index would assert whatever the ecosystem happened to hold that day.
type roundTripper struct {
	status int
	body   string
	err    error
}

func (r roundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    request,
	}, nil
}

func fakeIndex(rt roundTripper) Index {
	return Index{HTTP: &http.Client{Transport: rt}, URL: "https://example.test/index.json"}
}

const indexBody = `{
  "version": 1,
  "plugins": [
    {"name": "linear", "kind": "source", "summary": "Linear issues and comments", "coordinate": "github.com/jdoe/lore-linear@v0.3.1"},
    {"name": "acme-crm", "kind": "source", "summary": "Deals and accounts", "coordinate": "github.com/acme/lore-crm@v2.0.1"},
    {"name": "together", "kind": "provider", "summary": "Together embeddings", "coordinate": "github.com/acme/lore-together@v0.1.0"}
  ]
}`

func TestFetchReadsTheIndex(t *testing.T) {
	entries, _, err := fakeIndex(roundTripper{status: http.StatusOK, body: indexBody}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("Fetch() returned %d entries, want 3", len(entries))
	}
	want := Entry{
		Name:       "linear",
		Kind:       "source",
		Summary:    "Linear issues and comments",
		Coordinate: "github.com/jdoe/lore-linear@v0.3.1",
	}
	if entries[0] != want {
		t.Errorf("first entry = %+v, want %+v", entries[0], want)
	}
}

// Name, summary and kind are all searched because a user knows one of the
// three: the tool they use, what it does, or what they need it to be.
func TestMatchSearchesNameSummaryAndKind(t *testing.T) {
	entries, _, err := fakeIndex(roundTripper{status: http.StatusOK, body: indexBody}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}

	cases := map[string][]string{
		"linear":   {"linear"},                         // name
		"LINEAR":   {"linear"},                         // case folded
		"deals":    {"acme-crm"},                       // summary
		"provider": {"together"},                       // kind
		"source":   {"linear", "acme-crm"},             // kind, several
		"":         {"linear", "acme-crm", "together"}, // an empty query is everything
		"jira":     nil,                                // no match is not an error
	}

	for query, want := range cases {
		matched := Match(entries, query)
		got := make([]string, 0, len(matched))
		for _, e := range matched {
			got = append(got, e.Name)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("Match(%q) = %v, want %v", query, got, want)
		}
	}
}

// An empty index is the honest state of a young ecosystem, so it must reach the
// caller as data it can explain rather than as a failure.
func TestFetchReportsAnEmptyIndexAsData(t *testing.T) {
	entries, _, err := fakeIndex(roundTripper{status: http.StatusOK, body: `{"version": 1, "plugins": []}`}).
		Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() = %v, want an empty index to be readable", err)
	}
	if len(entries) != 0 {
		t.Errorf("Fetch() = %+v, want no entries", entries)
	}
}

func TestFetchReportsAnUnreachableIndex(t *testing.T) {
	offline := errors.New("dial tcp: no route to host")

	_, _, err := fakeIndex(roundTripper{err: offline}).Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() succeeded with no network")
	}
	if internalerror.KindOf(err) != internalerror.KindPrecondition {
		t.Errorf("kind = %v, want precondition", internalerror.KindOf(err))
	}
	for _, want := range []string{"unreachable", "https://example.test/index.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	if !errors.Is(err, offline) {
		t.Errorf("error = %v, want it to wrap the transport failure", err)
	}
}

func TestFetchRejectsAnUnusableIndex(t *testing.T) {
	cases := map[string]struct {
		rt   roundTripper
		want string
	}{
		"missing": {rt: roundTripper{status: http.StatusNotFound, body: "not found"}, want: "nothing published at"},
		"garbage": {rt: roundTripper{status: http.StatusOK, body: "<html>proxy error</html>"}, want: "not a readable index"},
		"future schema": {
			rt:   roundTripper{status: http.StatusOK, body: `{"version": 2, "plugins": []}`},
			want: "is version 2",
		},
	}

	for name, c := range cases {
		_, _, err := fakeIndex(c.rt).Fetch(context.Background())
		if err == nil {
			t.Errorf("%s: Fetch() succeeded", name)
			continue
		}
		if message := internalerror.MessageOf(err); !strings.Contains(message, c.want) {
			t.Errorf("%s: message = %q, want it to mention %q", name, message, c.want)
		}
	}
}

// An index the cap cuts short is not a JSON mistake, and reporting it as one
// sends the reader looking for a syntax error that is not there.
func TestFetchRefusesAnOversizedIndexAsTooLarge(t *testing.T) {
	oversized := roundTripper{status: http.StatusOK, body: strings.Repeat("a", plugindist.MaxMetadataBytes+1)}

	_, _, err := fakeIndex(oversized).Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() accepted an index larger than the cap")
	}
	message := internalerror.MessageOf(err)
	if !strings.Contains(message, "larger than") {
		t.Errorf("message = %q, want it to refuse the index as too large", message)
	}
	if strings.Contains(message, "not a readable index") {
		t.Errorf("message = %q, want a size refusal rather than a parse failure", message)
	}
}

func TestFetchReadsAnIndexServedOverTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(indexBody))
	}))
	t.Cleanup(server.Close)

	index := Index{HTTP: server.Client(), URL: server.URL + "/index.json"}
	entries, _, err := index.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("Fetch() returned %d entries, want 3", len(entries))
	}
}

func TestFetchRefusesAPlaintextIndexURL(t *testing.T) {
	var hits atomic.Int64
	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(indexBody))
	}))
	t.Cleanup(plaintext.Close)

	index := Index{HTTP: plaintext.Client(), URL: plaintext.URL + "/index.json"}
	entries, _, err := index.Fetch(context.Background())
	if err == nil {
		t.Fatalf("Fetch() read %d entries over plaintext http, want a refusal", len(entries))
	}
	message := internalerror.MessageOf(err)
	if !strings.Contains(message, "plugin traffic stays on https") {
		t.Errorf("message = %q, want it to refuse the plaintext index", message)
	}
	if internalerror.KindOf(err) != internalerror.KindPrecondition {
		t.Errorf("kind = %v, want precondition", internalerror.KindOf(err))
	}
	if served := hits.Load(); served != 0 {
		t.Errorf("the plaintext endpoint served %d requests, want none to leave", served)
	}
}

func TestFetchRefusesAnIndexRedirectOffHTTPS(t *testing.T) {
	var hits atomic.Int64
	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(indexBody))
	}))
	t.Cleanup(plaintext.Close)

	downgrade := plaintext.URL + "/index.json"
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, downgrade, http.StatusFound)
	}))
	t.Cleanup(secure.Close)

	index := Index{HTTP: secure.Client(), URL: secure.URL + "/index.json"}
	entries, _, err := index.Fetch(context.Background())
	if err == nil {
		t.Fatalf("Fetch() read %d entries after a downgrade, want a refusal", len(entries))
	}
	message := internalerror.MessageOf(err)
	if !strings.Contains(message, "refusing a redirect to "+downgrade) {
		t.Errorf("message = %q, want it to name the refused hop", message)
	}
	if strings.Contains(message, "unreachable") {
		t.Errorf("message = %q, want a refused index rather than an unreachable one", message)
	}
	if served := hits.Load(); served != 0 {
		t.Errorf("the plaintext endpoint served %d requests, want none", served)
	}
}

// The default index is a URL a user may have to open by hand when the search
// says it is unreachable, so it must be a real, readable location.
func TestDefaultIndexURLIsAnHTTPSURL(t *testing.T) {
	if !strings.HasPrefix(DefaultIndexURL, "https://") || !strings.HasSuffix(DefaultIndexURL, ".json") {
		t.Errorf("DefaultIndexURL = %q, want an https URL naming a JSON file", DefaultIndexURL)
	}
}

func indexEntry(name, kind, summary, coordinate string) string {
	return `{"name": "` + name + `", "kind": "` + kind + `", "summary": "` + summary +
		`", "coordinate": "` + coordinate + `"}`
}

func indexOf(entries ...string) string {
	return `{"version": 1, "plugins": [` + strings.Join(entries, ",") + `]}`
}

const goodCoordinate = "github.com/jdoe/lore-linear@v0.3.1"

// A search prints the coordinate the user is told to hand to `lore plugin
// install`, so an entry that can rewrite its own rendered row is a coordinate
// swap, not a cosmetic defect.
func TestFetchSkipsEntriesItCannotRender(t *testing.T) {
	overCap := strings.Repeat("a", maxEntryFieldBytes+1)
	cases := map[string]string{
		"carriage return in summary": indexEntry("linear", "source",
			`Linear issues\r linear  source  github.com/evil/lore-linear@v9`, goodCoordinate),
		"ansi escape in summary": indexEntry("linear", "source",
			`Linear issues\u001b[1A\u001b[2K`, goodCoordinate),
		"newline in summary": indexEntry("linear", "source", `Linear\nissues`, goodCoordinate),
		"tab in summary":     indexEntry("linear", "source", `Linear\tissues`, goodCoordinate),
		"delete in summary":  indexEntry("linear", "source", `Linear issues\u007f`, goodCoordinate),
		"c1 control in name": indexEntry(`linear\u0085`, "source", "Linear issues", goodCoordinate),
		"invalid utf8":       indexEntry("linear", "source", "Linear \xff\xfe issues", goodCoordinate),
		"lone surrogate":     indexEntry("linear", "source", `Linear \ud800 issues`, goodCoordinate),
		"escape in coordinate": indexEntry("linear", "source", "Linear issues",
			`github.com/jdoe/lore-linear@v0.3.1\u001b[1A`),
		"upper case name":         indexEntry("Linear", "source", "Linear issues", goodCoordinate),
		"path separator name":     indexEntry("lore/linear", "source", "Linear issues", goodCoordinate),
		"dotted name":             indexEntry("lore.linear", "source", "Linear issues", goodCoordinate),
		"spaced name":             indexEntry("lore linear", "source", "Linear issues", goodCoordinate),
		"empty name":              indexEntry("", "source", "Linear issues", goodCoordinate),
		"unknown kind":            indexEntry("linear", "sink", "Linear issues", goodCoordinate),
		"empty kind":              indexEntry("linear", "", "Linear issues", goodCoordinate),
		"over cap summary":        indexEntry("linear", "source", overCap, goodCoordinate),
		"over cap name":           indexEntry(overCap, "source", "Linear issues", goodCoordinate),
		"over cap coordinate":     indexEntry("linear", "source", "Linear issues", overCap),
		"spaced coordinate":       indexEntry("linear", "source", "Linear issues", `github.com/jdoe/lore-linear @v0.3.1`),
		"empty coordinate":        indexEntry("linear", "source", "Linear issues", ""),
		"non breaking space":      indexEntry("linear", "source", `Linear\u00a0issues`, goodCoordinate),
		"right to left override":  indexEntry("linear", "source", `Linear \u202eissues`, goodCoordinate),
		"flag leading coordinate": indexEntry("linear", "source", "Linear issues", `-C/x@v1.0.0`),
		"dash coordinate":         indexEntry("linear", "source", "Linear issues", `--flag`),
		"kind is a number":        `{"name": "linear", "kind": 5, "summary": "Linear issues", "coordinate": "` + goodCoordinate + `"}`,
		"entry is a string":       `"linear"`,
		"entry is an array":       `["linear", "source"]`,
		"entry is null":           `null`,
	}

	for name, entry := range cases {
		entries, skipped, err := fakeIndex(roundTripper{status: http.StatusOK, body: indexOf(entry)}).
			Fetch(context.Background())
		if err != nil {
			t.Errorf("%s: Fetch() = %v, want one unusable entry skipped rather than a refused index", name, err)
			continue
		}
		if len(entries) != 0 {
			t.Errorf("%s: Fetch() kept %+v, want the entry refused", name, entries)
		}
		if skipped != 1 {
			t.Errorf("%s: skipped = %d, want 1", name, skipped)
		}
	}
}

// One bad third-party entry must not take `lore plugin search` down for every
// user, and it must not vanish quietly either.
func TestFetchKeepsGoodEntriesBesideSkippedOnes(t *testing.T) {
	body := indexOf(
		indexEntry("linear", "source", "Linear issues and comments", goodCoordinate),
		indexEntry("linear", "source", `Linear issues\r spoofed`, "github.com/evil/lore-linear@v9"),
		indexEntry("SHOUTING", "source", "Bad name", "github.com/evil/lore-shout@v9"),
		indexEntry("together", "provider", "Together embeddings", "github.com/acme/lore-together@v0.1.0"),
	)

	entries, skipped, err := fakeIndex(roundTripper{status: http.StatusOK, body: body}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() = %v, want the readable entries", err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2 so the operator is told what is missing", skipped)
	}
	want := []Entry{
		{Name: "linear", Kind: "source", Summary: "Linear issues and comments", Coordinate: goodCoordinate},
		{Name: "together", Kind: "provider", Summary: "Together embeddings", Coordinate: "github.com/acme/lore-together@v0.1.0"},
	}
	if len(entries) != len(want) {
		t.Fatalf("Fetch() = %+v, want %+v", entries, want)
	}
	for i, e := range entries {
		if e != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, e, want[i])
		}
	}
}

func TestFetchSkipsNothingInAWellFormedIndex(t *testing.T) {
	entries, skipped, err := fakeIndex(roundTripper{status: http.StatusOK, body: indexBody}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want a well-formed index to lose nothing", skipped)
	}
	want := []Entry{
		{Name: "linear", Kind: "source", Summary: "Linear issues and comments", Coordinate: "github.com/jdoe/lore-linear@v0.3.1"},
		{Name: "acme-crm", Kind: "source", Summary: "Deals and accounts", Coordinate: "github.com/acme/lore-crm@v2.0.1"},
		{Name: "together", Kind: "provider", Summary: "Together embeddings", Coordinate: "github.com/acme/lore-together@v0.1.0"},
	}
	if len(entries) != len(want) {
		t.Fatalf("Fetch() = %+v, want %+v", entries, want)
	}
	for i, e := range entries {
		if e != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, e, want[i])
		}
	}
}

func TestFetchAcceptsEveryKindTheSDKDefines(t *testing.T) {
	for _, kind := range []lore.Kind{lore.KindSource, lore.KindProvider, lore.KindCode} {
		body := indexOf(indexEntry("linear", string(kind), "Linear issues", goodCoordinate))
		entries, skipped, err := fakeIndex(roundTripper{status: http.StatusOK, body: body}).Fetch(context.Background())
		if err != nil {
			t.Errorf("%s: Fetch() = %v", kind, err)
			continue
		}
		if len(entries) != 1 || skipped != 0 {
			t.Errorf("%s: Fetch() = %+v, skipped %d, want the entry kept", kind, entries, skipped)
		}
	}
}

// A field one byte under the cap is data, not an attack: the cap must not
// quietly delete ordinary long summaries.
func TestFetchKeepsAFieldAtTheCap(t *testing.T) {
	atCap := strings.Repeat("a", maxEntryFieldBytes)
	body := indexOf(indexEntry("linear", "source", atCap, goodCoordinate))

	entries, skipped, err := fakeIndex(roundTripper{status: http.StatusOK, body: body}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	if len(entries) != 1 || skipped != 0 {
		t.Fatalf("Fetch() = %+v, skipped %d, want a summary exactly at the cap kept", entries, skipped)
	}
	if entries[0].Summary != atCap {
		t.Errorf("summary = %q, want it kept whole rather than trimmed", entries[0].Summary)
	}
}

// A multibyte summary is measured in bytes, and it must survive intact: the
// refusal is a refusal, never a sanitising rewrite.
func TestFetchKeepsMultibyteTextWhole(t *testing.T) {
	body := indexOf(indexEntry("nihongo", "source", `日本語のドキュメント`, goodCoordinate))

	entries, skipped, err := fakeIndex(roundTripper{status: http.StatusOK, body: body}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	if len(entries) != 1 || skipped != 0 {
		t.Fatalf("Fetch() = %+v, skipped %d, want printable multibyte text kept", entries, skipped)
	}
	if entries[0].Summary != "日本語のドキュメント" {
		t.Errorf("summary = %q, want it unchanged", entries[0].Summary)
	}
}

// A refused index skipped nothing: it was never read, and a count above zero
// would tell the operator entries were dropped when the whole document was.
func TestFetchReportsNoSkippedEntriesWhenItRefusesTheIndex(t *testing.T) {
	cases := map[string]roundTripper{
		"unreachable":   {err: errors.New("dial tcp: no route to host")},
		"unreadable":    {status: http.StatusOK, body: "<html>proxy error</html>"},
		"future schema": {status: http.StatusOK, body: `{"version": 2, "plugins": []}`},
	}

	for name, rt := range cases {
		entries, skipped, err := fakeIndex(rt).Fetch(context.Background())
		if err == nil {
			t.Errorf("%s: Fetch() = %+v, want a refusal", name, entries)
			continue
		}
		if skipped != 0 {
			t.Errorf("%s: skipped = %d, want 0 for a document that was never read", name, skipped)
		}
	}
}

// A document whose own shape is wrong was never an index, so there is no entry
// to skip and nothing partial to show.
func TestFetchStillRefusesADocumentWhoseShapeIsWrong(t *testing.T) {
	cases := map[string]string{
		"plugins is an object": `{"version": 1, "plugins": {"linear": {}}}`,
		"plugins is a string":  `{"version": 1, "plugins": "linear"}`,
		"plugins is a number":  `{"version": 1, "plugins": 3}`,
		"version is a string":  `{"version": "1", "plugins": []}`,
		"not json at all":      `<html>proxy error</html>`,
	}

	for name, body := range cases {
		entries, skipped, err := fakeIndex(roundTripper{status: http.StatusOK, body: body}).Fetch(context.Background())
		if err == nil {
			t.Errorf("%s: Fetch() = %+v, want the document refused", name, entries)
			continue
		}
		if skipped != 0 {
			t.Errorf("%s: skipped = %d, want 0", name, skipped)
		}
	}
}

// One entry the decoder cannot read is that entry's problem, not the index's.
func TestFetchSkipsAnEntryItCannotDecodeAndKeepsTheRest(t *testing.T) {
	body := indexOf(
		indexEntry("linear", "source", "Linear issues and comments", goodCoordinate),
		`{"name": "together", "kind": 5, "summary": "Together embeddings", "coordinate": "github.com/acme/lore-together@v0.1.0"}`,
		`{"name": ["acme-crm"], "kind": "source", "summary": "Deals", "coordinate": "github.com/acme/lore-crm@v2.0.1"}`,
		indexEntry("git", "code", "One local clone", "github.com/acme/lore-git@v1.0.0"),
	)

	entries, skipped, err := fakeIndex(roundTripper{status: http.StatusOK, body: body}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() = %v, want the decodable entries", err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
	if len(entries) != 2 || entries[0].Name != "linear" || entries[1].Name != "git" {
		t.Errorf("Fetch() = %+v, want linear and git in index order", entries)
	}
}
