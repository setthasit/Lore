package plugbuild

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
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
	entries, err := fakeIndex(roundTripper{status: http.StatusOK, body: indexBody}).Fetch(context.Background())
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
	entries, err := fakeIndex(roundTripper{status: http.StatusOK, body: indexBody}).Fetch(context.Background())
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
	entries, err := fakeIndex(roundTripper{status: http.StatusOK, body: `{"version": 1, "plugins": []}`}).
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

	_, err := fakeIndex(roundTripper{err: offline}).Fetch(context.Background())
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
		"missing": {rt: roundTripper{status: http.StatusNotFound, body: "not found"}, want: "HTTP 404"},
		"garbage": {rt: roundTripper{status: http.StatusOK, body: "<html>proxy error</html>"}, want: "not a readable index"},
		"future schema": {
			rt:   roundTripper{status: http.StatusOK, body: `{"version": 2, "plugins": []}`},
			want: "is version 2",
		},
	}

	for name, c := range cases {
		_, err := fakeIndex(c.rt).Fetch(context.Background())
		if err == nil {
			t.Errorf("%s: Fetch() succeeded", name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %q, want it to mention %q", name, err, c.want)
		}
	}
}

// The default index is a URL a user may have to open by hand when the search
// says it is unreachable, so it must be a real, readable location.
func TestDefaultIndexURLIsAnHTTPSURL(t *testing.T) {
	if !strings.HasPrefix(DefaultIndexURL, "https://") || !strings.HasSuffix(DefaultIndexURL, ".json") {
		t.Errorf("DefaultIndexURL = %q, want an https URL naming a JSON file", DefaultIndexURL)
	}
}
