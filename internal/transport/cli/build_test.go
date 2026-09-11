package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/plugbuild"
)

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

// plugin search reads DefaultIndexURL through http.DefaultClient, so the fake transport goes on that client.
func fakeIndex(t *testing.T, index fakeIndexTransport) {
	t.Helper()

	previous := http.DefaultClient.Transport
	http.DefaultClient.Transport = index
	t.Cleanup(func() { http.DefaultClient.Transport = previous })
}

func TestBuildRefusesACoordinateWithNoDerivablePackageAsABadRequest(t *testing.T) {
	res := run(t, nil, "build", "--with", "github.com/acme/lore-acme.crm@v1.0.0")
	if res.exitCode != exitBadRequest {
		t.Errorf("exit = %d, want %d; stderr = %q", res.exitCode, exitBadRequest, res.stderr)
	}
}

func TestBuildWithoutAToolchainIsAPreconditionFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	res := run(t, nil, "build", "--with", "github.com/jdoe/lore-linear@v0.3.1")
	if res.exitCode != exitPrecondition {
		t.Errorf("exit = %d, want %d; stderr = %q", res.exitCode, exitPrecondition, res.stderr)
	}
}

func TestBuildBuildsAgainstTheEngineTheFlagNames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flag    []string
		fetched string
	}{
		{
			name:    "the flag pins the engine",
			flag:    []string{"--engine", "v0.4.0"},
			fetched: "v0.4.0",
		},
		{
			name:    "no flag leaves the version to the running binary",
			fetched: "latest",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invocations := fakeToolchain(t)

			args := append([]string{
				"build",
				"--with", "github.com/jdoe/lore-linear@v0.3.1",
				"--output", filepath.Join(t.TempDir(), "lore"),
			}, tc.flag...)
			res := run(t, nil, args...)
			if res.exitCode != exitOK {
				t.Fatalf("exit = %d, want %d; stderr = %q", res.exitCode, exitOK, res.stderr)
			}

			raw, err := os.ReadFile(invocations)
			if err != nil {
				t.Fatalf("read the recorded go invocations: %v", err)
			}
			calls := strings.Split(strings.TrimSpace(string(raw)), "\n")

			want := "get github.com/setthasit/Lore@" + tc.fetched
			if !slices.Contains(calls, want) {
				t.Errorf("go was called as\n%s\nwant %q among them", strings.Join(calls, "\n"), want)
			}
		})
	}
}

func fakeToolchain(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	invocations := filepath.Join(dir, "invocations")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> \"" + invocations + "\"\n" +
		"case \"$1\" in\n" +
		"list) echo v0.4.2 ;;\n" +
		"build) printf '#!/bin/sh\\necho no plugins\\n' > \"$3\"; /bin/chmod +x \"$3\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatalf("write a fake toolchain: %v", err)
	}
	t.Setenv("PATH", dir)
	return invocations
}

func TestPluginSearchPrintsEveryColumnAMatchNeeds(t *testing.T) {
	fakeIndex(t, fakeIndexTransport{body: searchIndexBody})

	res := run(t, nil, "plugin", "search", "linear")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{
		"linear", "source", "Linear issues and comments", "github.com/jdoe/lore-linear@v0.3.1",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", res.stdout, want)
		}
	}
	if strings.Contains(res.stdout, "together") {
		t.Errorf("stdout = %q, want only the matching plugin", res.stdout)
	}
}

func TestPluginSearchMatchesNameSummaryAndKind(t *testing.T) {
	fakeIndex(t, fakeIndexTransport{body: searchIndexBody})

	for query, want := range map[string]string{
		"LINEAR":     "linear",
		"embeddings": "together",
		"provider":   "together",
	} {
		res := run(t, nil, "plugin", "search", query)
		if res.exitCode != exitOK {
			t.Errorf("search %q: exit = %d, stderr = %q", query, res.exitCode, res.stderr)
			continue
		}
		if !strings.Contains(res.stdout, want) {
			t.Errorf("search %q printed %q, want %q listed", query, res.stdout, want)
		}
	}
}

func TestPluginSearchReportsNoMatch(t *testing.T) {
	fakeIndex(t, fakeIndexTransport{body: searchIndexBody})

	res := run(t, nil, "plugin", "search", "jira")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "no plugin matches") || !strings.Contains(res.stdout, "2 plugins") {
		t.Errorf("stdout = %q, want it to say nothing matched and how much was searched", res.stdout)
	}
}

func TestPluginSearchReportsAnEmptyIndex(t *testing.T) {
	fakeIndex(t, fakeIndexTransport{body: `{"version": 1, "plugins": []}`})

	res := run(t, nil, "plugin", "search", "linear")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "the plugin index is empty") {
		t.Errorf("stdout = %q, want it to say the index is empty", res.stdout)
	}
}

func TestPluginSearchSaysHowManyEntriesItLeftOut(t *testing.T) {
	fakeIndex(t, fakeIndexTransport{body: `{
  "version": 1,
  "plugins": [
    {"name": "linear", "kind": "source", "summary": "Linear issues and comments", "coordinate": "github.com/jdoe/lore-linear@v0.3.1"},
    {"name": "linear", "kind": "source", "summary": "Linear issues\r linear  source  github.com/evil/lore-linear@v9", "coordinate": "github.com/evil/lore-linear@v9"},
    {"name": "LINEAR-SHOUT", "kind": "source", "summary": "Bad name", "coordinate": "github.com/evil/lore-shout@v9"}
  ]
}`})

	res := run(t, nil, "plugin", "search", "linear")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "left out 2 entries") {
		t.Errorf("stdout = %q, want it to say how many entries were left out", res.stdout)
	}
	if strings.Contains(res.stdout, "github.com/evil/lore-linear@v9") {
		t.Errorf("stdout = %q, want the refused entry absent from the table", res.stdout)
	}
	if !strings.Contains(res.stdout, "github.com/jdoe/lore-linear@v0.3.1") {
		t.Errorf("stdout = %q, want the usable entry still listed", res.stdout)
	}
}

func TestPluginSearchSaysNothingAboutSkippedEntriesWhenNoneAre(t *testing.T) {
	fakeIndex(t, fakeIndexTransport{body: searchIndexBody})

	res := run(t, nil, "plugin", "search", "linear")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if strings.Contains(res.stdout, "left out") {
		t.Errorf("stdout = %q, want no notice for a well-formed index", res.stdout)
	}
}

func TestPluginSearchDoesNotCallAnAllRefusedIndexEmpty(t *testing.T) {
	fakeIndex(t, fakeIndexTransport{body: `{
  "version": 1,
  "plugins": [
    {"name": "linear", "kind": "source", "summary": "Linear issues\r spoofed", "coordinate": "github.com/evil/lore-linear@v9"}
  ]
}`})

	res := run(t, nil, "plugin", "search", "linear")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "left out 1 entry") {
		t.Errorf("stdout = %q, want it to say the entry was left out", res.stdout)
	}
	if strings.Contains(res.stdout, "the plugin index is empty") {
		t.Errorf("stdout = %q, want no claim that nothing is published", res.stdout)
	}
}

func TestPluginSearchReportsAnUnreachableIndexAsAPreconditionFailure(t *testing.T) {
	fakeIndex(t, fakeIndexTransport{err: errors.New("dial tcp: no route to host")})

	res := run(t, nil, "plugin", "search", "linear")
	if res.exitCode != exitPrecondition {
		t.Errorf("exit = %d, want %d; stderr = %q", res.exitCode, exitPrecondition, res.stderr)
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
