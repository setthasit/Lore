package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/plugbuild"
)

// fakeIndexTransport serves the plugin index from memory: a search test that
// reached the real index would assert whatever the ecosystem holds today.
type fakeIndexTransport struct {
	body string
	err  error
}

func (f fakeIndexTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Request:    request,
	}, nil
}

const searchIndexBody = `{
  "version": 1,
  "plugins": [
    {"name": "linear", "kind": "source", "summary": "Linear issues and comments", "coordinate": "github.com/jdoe/lore-linear@v0.3.1"},
    {"name": "together", "kind": "provider", "summary": "Together embeddings", "coordinate": "github.com/acme/lore-together@v0.1.0"}
  ]
}`

func searchCommand(transport http.RoundTripper) *cobra.Command {
	return newPluginSearchCommand(plugbuild.Index{
		HTTP: &http.Client{Transport: transport},
		URL:  "https://example.test/index.json",
	})
}

func runCommand(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestBuildRefusesACoordinateWithNoDerivablePackageAsABadRequest(t *testing.T) {
	_, err := runCommand(t, newBuildCommand(), "--with", "github.com/acme/lore-acme.crm@v1.0.0")
	if err == nil {
		t.Fatal("build accepted a coordinate whose package name cannot be derived")
	}
	if code := Report(io.Discard, err); code != exitBadRequest {
		t.Errorf("exit = %d, want %d", code, exitBadRequest)
	}
}

func TestBuildWithoutAToolchainIsAPreconditionFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := runCommand(t, newBuildCommand(), "--with", "github.com/jdoe/lore-linear@v0.3.1")
	if err == nil {
		t.Fatal("build succeeded with no Go toolchain on PATH")
	}
	if code := Report(io.Discard, err); code != exitPrecondition {
		t.Errorf("exit = %d, want %d", code, exitPrecondition)
	}
}

func TestPluginSearchPrintsEveryColumnAMatchNeeds(t *testing.T) {
	out, err := runCommand(t, searchCommand(fakeIndexTransport{body: searchIndexBody}), "linear")
	if err != nil {
		t.Fatalf("search = %v", err)
	}
	for _, want := range []string{
		"linear", "source", "Linear issues and comments", "github.com/jdoe/lore-linear@v0.3.1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "together") {
		t.Errorf("stdout = %q, want only the matching plugin", out)
	}
}

// A user knows one of three things about the plugin they want: what it is
// called, what it does, or what it has to be. All three are matched.
func TestPluginSearchMatchesNameSummaryAndKind(t *testing.T) {
	for query, want := range map[string]string{
		"LINEAR":     "linear",   // name, case folded
		"embeddings": "together", // summary
		"provider":   "together", // kind
	} {
		out, err := runCommand(t, searchCommand(fakeIndexTransport{body: searchIndexBody}), query)
		if err != nil {
			t.Errorf("search %q = %v", query, err)
			continue
		}
		if !strings.Contains(out, want) {
			t.Errorf("search %q printed %q, want %q listed", query, out, want)
		}
	}
}

func TestPluginSearchReportsNoMatch(t *testing.T) {
	out, err := runCommand(t, searchCommand(fakeIndexTransport{body: searchIndexBody}), "jira")
	if err != nil {
		t.Fatalf("search = %v", err)
	}
	if !strings.Contains(out, "no plugin matches") || !strings.Contains(out, "2 plugins") {
		t.Errorf("stdout = %q, want it to say nothing matched and how much was searched", out)
	}
}

// An empty index is the honest state of a young ecosystem, and printing nothing
// would read as a broken command.
func TestPluginSearchReportsAnEmptyIndex(t *testing.T) {
	out, err := runCommand(t, searchCommand(fakeIndexTransport{body: `{"version": 1, "plugins": []}`}), "linear")
	if err != nil {
		t.Fatalf("search = %v", err)
	}
	if !strings.Contains(out, "the plugin index is empty") {
		t.Errorf("stdout = %q, want it to say the index is empty", out)
	}
}

// The index is fetched from a first-party host, but every entry in it describes
// somebody else's plugin, so a refused entry is news the operator needs.
func TestPluginSearchSaysHowManyEntriesItLeftOut(t *testing.T) {
	out, err := runCommand(t, searchCommand(fakeIndexTransport{body: `{
  "version": 1,
  "plugins": [
    {"name": "linear", "kind": "source", "summary": "Linear issues and comments", "coordinate": "github.com/jdoe/lore-linear@v0.3.1"},
    {"name": "linear", "kind": "source", "summary": "Linear issues\r linear  source  github.com/evil/lore-linear@v9", "coordinate": "github.com/evil/lore-linear@v9"},
    {"name": "LINEAR-SHOUT", "kind": "source", "summary": "Bad name", "coordinate": "github.com/evil/lore-shout@v9"}
  ]
}`}), "linear")
	if err != nil {
		t.Fatalf("search = %v", err)
	}
	if !strings.Contains(out, "left out 2 entries") {
		t.Errorf("stdout = %q, want it to say how many entries were left out", out)
	}
	if strings.Contains(out, "github.com/evil/lore-linear@v9") {
		t.Errorf("stdout = %q, want the refused entry absent from the table", out)
	}
	if !strings.Contains(out, "github.com/jdoe/lore-linear@v0.3.1") {
		t.Errorf("stdout = %q, want the usable entry still listed", out)
	}
}

func TestPluginSearchSaysNothingAboutSkippedEntriesWhenNoneAre(t *testing.T) {
	out, err := runCommand(t, searchCommand(fakeIndexTransport{body: searchIndexBody}), "linear")
	if err != nil {
		t.Fatalf("search = %v", err)
	}
	if strings.Contains(out, "left out") {
		t.Errorf("stdout = %q, want no notice for a well-formed index", out)
	}
}

// "nothing is published yet" would be a lie about an index that published
// entries this build refused: the operator must be told which of the two it is.
func TestPluginSearchDoesNotCallAnAllRefusedIndexEmpty(t *testing.T) {
	out, err := runCommand(t, searchCommand(fakeIndexTransport{body: `{
  "version": 1,
  "plugins": [
    {"name": "linear", "kind": "source", "summary": "Linear issues\r spoofed", "coordinate": "github.com/evil/lore-linear@v9"}
  ]
}`}), "linear")
	if err != nil {
		t.Fatalf("search = %v", err)
	}
	if !strings.Contains(out, "left out 1 entry") {
		t.Errorf("stdout = %q, want it to say the entry was left out", out)
	}
	if strings.Contains(out, "the plugin index is empty") {
		t.Errorf("stdout = %q, want no claim that nothing is published", out)
	}
}

func TestPluginSearchReportsAnUnreachableIndexAsAPreconditionFailure(t *testing.T) {
	_, err := runCommand(t, searchCommand(fakeIndexTransport{err: errors.New("dial tcp: no route to host")}), "linear")
	if err == nil {
		t.Fatal("search succeeded with no network")
	}
	if code := Report(io.Discard, err); code != exitPrecondition {
		t.Errorf("exit = %d, want %d", code, exitPrecondition)
	}
}

func TestRenderSearchResultsPadsColumnsByRunesNotBytesOrDisplayWidth(t *testing.T) {
	var out bytes.Buffer
	renderSearchResults(&out, []plugbuild.Entry{
		{Name: "linear", Kind: "source", Summary: "Linear issues", Coordinate: "github.com/jdoe/lore-linear@v0.3.1"},
		{Name: "t", Kind: "provider", Summary: "Together embeddings", Coordinate: "github.com/a/t@v0.1.0"},
		{Name: "ünïcode", Kind: "source", Summary: "Multibyte name", Coordinate: "github.com/x/u@v2"},
		{Name: "ünïcode-wïdest", Kind: "provider", Summary: "Widest is multibyte", Coordinate: "github.com/x/w@v1"},
		{Name: "日本語", Kind: "source", Summary: "Double-width runes count as one", Coordinate: "github.com/x/j@v1"},
	})

	want := "NAME            KIND      COORDINATE                          SUMMARY\n" +
		"linear          source    github.com/jdoe/lore-linear@v0.3.1  Linear issues\n" +
		"t               provider  github.com/a/t@v0.1.0               Together embeddings\n" +
		"ünïcode         source    github.com/x/u@v2                   Multibyte name\n" +
		"ünïcode-wïdest  provider  github.com/x/w@v1                   Widest is multibyte\n" +
		"日本語             source    github.com/x/j@v1                   Double-width runes count as one\n"
	if got := out.String(); got != want {
		t.Errorf("renderSearchResults() =\n%q\nwant\n%q", got, want)
	}
}
