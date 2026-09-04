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

func serveIndex(t *testing.T, transport http.RoundTripper) {
	t.Helper()

	restore := pluginIndex
	pluginIndex = plugbuild.Index{HTTP: &http.Client{Transport: transport}, URL: "https://example.test/index.json"}
	t.Cleanup(func() { pluginIndex = restore })
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

// The build never starts: a coordinate whose package cannot be derived is
// rejected before a toolchain is looked for, so the user fixes the flag rather
// than reading a compile error in generated code.
func TestBuildCommandAsksForAnExplicitPackage(t *testing.T) {
	_, err := runCommand(t, newBuildCommand(), "--with", "github.com/acme/lore-acme.crm@v1.0.0")
	if err == nil {
		t.Fatal("build accepted a coordinate whose package name cannot be derived")
	}
	if got := actionableMessage(err); !strings.Contains(got, "=acmecrm") {
		t.Errorf("error = %q, want the =<package> suffix spelled out", got)
	}
	if code := report(io.Discard, err); code != exitBadRequest {
		t.Errorf("exit = %d, want %d", code, exitBadRequest)
	}
}

func TestBuildCommandWithoutAToolchainSaysWhyItNeedsOne(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := runCommand(t, newBuildCommand(), "--with", "github.com/jdoe/lore-linear@v0.3.1")
	if err == nil {
		t.Fatal("build succeeded with no Go toolchain on PATH")
	}
	message := actionableMessage(err)
	for _, want := range []string{"Go toolchain", "compile-time type safety", "lore plugin install"} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want it to mention %q", message, want)
		}
	}
	if code := report(io.Discard, err); code != exitPrecondition {
		t.Errorf("exit = %d, want %d", code, exitPrecondition)
	}
}

func TestPluginSearchPrintsEveryColumnAMatchNeeds(t *testing.T) {
	serveIndex(t, fakeIndexTransport{body: searchIndexBody})

	out, err := runCommand(t, newPluginSearchCommand(), "linear")
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
	serveIndex(t, fakeIndexTransport{body: searchIndexBody})

	for query, want := range map[string]string{
		"LINEAR":     "linear",   // name, case folded
		"embeddings": "together", // summary
		"provider":   "together", // kind
	} {
		out, err := runCommand(t, newPluginSearchCommand(), query)
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
	serveIndex(t, fakeIndexTransport{body: searchIndexBody})

	out, err := runCommand(t, newPluginSearchCommand(), "jira")
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
	serveIndex(t, fakeIndexTransport{body: `{"version": 1, "plugins": []}`})

	out, err := runCommand(t, newPluginSearchCommand(), "linear")
	if err != nil {
		t.Fatalf("search = %v", err)
	}
	if !strings.Contains(out, "the plugin index is empty") {
		t.Errorf("stdout = %q, want it to say the index is empty", out)
	}
}

func TestPluginSearchReportsAnUnreachableIndex(t *testing.T) {
	serveIndex(t, fakeIndexTransport{err: errors.New("dial tcp: no route to host")})

	_, err := runCommand(t, newPluginSearchCommand(), "linear")
	if err == nil {
		t.Fatal("search succeeded with no network")
	}
	message := actionableMessage(err)
	if !strings.Contains(message, "unreachable") || !strings.Contains(message, "https://example.test/index.json") {
		t.Errorf("error = %q, want it to name the unreachable index", message)
	}
	if code := report(io.Discard, err); code != exitPrecondition {
		t.Errorf("exit = %d, want %d", code, exitPrecondition)
	}
}
