package plugbuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const fakeGo = "/nonexistent/bin/go"

type recordedRun struct {
	dir     string
	program string
	args    []string
}

func (r recordedRun) command() string {
	return strings.Join(append([]string{filepath.Base(r.program)}, r.args...), " ")
}

// fakeRunner stands in for the toolchain so the fast tests assert the exact
// command sequence without a compiler, a network or a minute of compilation.
type fakeRunner struct {
	runs   []recordedRun
	answer func(recordedRun) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, dir, program string, args ...string) (string, error) {
	run := recordedRun{dir: dir, program: program, args: args}
	f.runs = append(f.runs, run)
	if f.answer == nil {
		return "", nil
	}
	return f.answer(run)
}

func (f *fakeRunner) commands() []string {
	out := make([]string, 0, len(f.runs))
	for _, r := range f.runs {
		out = append(out, r.command())
	}
	return out
}

func TestBuildFetchesCompilesAndReadsTheArtifactBack(t *testing.T) {
	scratchParent, outputDir := t.TempDir(), t.TempDir()
	output := filepath.Join(outputDir, "lore")

	const listing = "NAME    KIND    ORIGIN   SUMMARY\nlinear  source  builtin  Linear issues\n"
	var generated string

	runner := &fakeRunner{}
	runner.answer = func(run recordedRun) (string, error) {
		// Reading the file at compile time proves the generated root reached the
		// scratch module, not merely that Render was called.
		if run.program == fakeGo && len(run.args) > 0 && run.args[0] == "build" {
			raw, err := os.ReadFile(filepath.Join(run.dir, generatedFile))
			if err != nil {
				return "", err
			}
			generated = string(raw)
		}
		if run.program == output {
			return listing, nil
		}
		return "", nil
	}

	var progress strings.Builder
	result, err := Build(context.Background(), Request{
		Coordinates: parseAll(t,
			"github.com/jdoe/lore-linear@v0.3.1",
			"github.com/acme/lore-crm/v2@v2.0.1=acmecrm"),
		Output:   output,
		Engine:   "v0.4.0",
		TempDir:  scratchParent,
		Progress: &progress,
		Runner:   runner,
		Go:       fakeGo,
	})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}

	want := []string{
		"go mod init " + scratchModule,
		"go get github.com/setthasit/Lore@v0.4.0",
		"go get github.com/acme/lore-crm/v2@v2.0.1",
		"go get github.com/jdoe/lore-linear@v0.3.1",
		"go mod download",
		"go build -o " + output + " .",
		filepath.Base(output) + " plugin list",
	}
	if got := runner.commands(); !slices.Equal(got, want) {
		t.Errorf("commands =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if generated != goldenRoot {
		t.Errorf("scratch module holds\n%s\nwant the generated composition root", generated)
	}
	if result.Output != output {
		t.Errorf("Output = %q, want %q", result.Output, output)
	}
	if result.Engine != "v0.4.0" {
		t.Errorf("Engine = %q, want the requested engine version", result.Engine)
	}
	if result.Plugins != listing {
		t.Errorf("Plugins = %q, want the artifact's own plugin list", result.Plugins)
	}
	if len(result.Added) != 2 || result.Added[0].Module != "github.com/acme/lore-crm/v2" {
		t.Errorf("Added = %+v, want both plugins in module order", result.Added)
	}
	if !strings.Contains(progress.String(), "compiling "+output) {
		t.Errorf("progress = %q, want the compile step announced", progress.String())
	}
	assertNoScratchModule(t, scratchParent)
}

func TestBuildRemovesTheScratchModuleWhenTheCompileFails(t *testing.T) {
	scratchParent := t.TempDir()
	output := filepath.Join(t.TempDir(), "lore")

	compileFailed := errors.New("exit status 1")
	runner := &fakeRunner{answer: func(run recordedRun) (string, error) {
		if len(run.args) > 0 && run.args[0] == "build" {
			return "plugin.go:9:2: undefined: lore.Connector", compileFailed
		}
		return "", nil
	}}

	_, err := Build(context.Background(), Request{
		Coordinates: parseAll(t, "github.com/jdoe/lore-linear@v0.3.1"),
		Output:      output,
		Engine:      "v0.4.0",
		TempDir:     scratchParent,
		Runner:      runner,
		Go:          fakeGo,
	})
	if err == nil {
		t.Fatal("Build() succeeded although the compile failed")
	}
	if internalerror.KindOf(err) != internalerror.KindPrecondition {
		t.Errorf("kind = %v, want precondition", internalerror.KindOf(err))
	}
	if !errors.Is(err, compileFailed) {
		t.Errorf("error = %v, want it to wrap the toolchain failure", err)
	}
	for _, want := range []string{"api version", "undefined: lore.Connector"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	assertNoScratchModule(t, scratchParent)
}

// A binary whose plugin set fails to register is not a build that succeeded:
// registration is where a misdeclared plugin is supposed to surface.
func TestBuildFailsWhenTheArtifactCannotListItsPlugins(t *testing.T) {
	scratchParent := t.TempDir()
	output := filepath.Join(t.TempDir(), "lore")

	runner := &fakeRunner{answer: func(run recordedRun) (string, error) {
		if run.program == output {
			return "lore: plugin \"linear\" speaks api_version 2, host speaks 1", errors.New("exit status 3")
		}
		return "", nil
	}}

	_, err := Build(context.Background(), Request{
		Coordinates: parseAll(t, "github.com/jdoe/lore-linear@v0.3.1"),
		Output:      output,
		Engine:      "v0.4.0",
		TempDir:     scratchParent,
		Runner:      runner,
		Go:          fakeGo,
	})
	if err == nil {
		t.Fatal("Build() reported success for a binary that does not run")
	}
	if !strings.Contains(err.Error(), "failed to register") {
		t.Errorf("error = %q, want it to say the plugin set failed to register", err)
	}
	assertNoScratchModule(t, scratchParent)
}

func TestBuildUsesReplaceInsteadOfFetching(t *testing.T) {
	scratchParent, local := t.TempDir(), t.TempDir()
	output := filepath.Join(t.TempDir(), "lore")

	runner := &fakeRunner{}
	_, err := Build(context.Background(), Request{
		Coordinates: parseAll(t, "github.com/jdoe/lore-linear@v0.3.1"),
		Output:      output,
		Engine:      develVersion,
		Replace:     map[string]string{engineModule: local},
		TempDir:     scratchParent,
		Runner:      runner,
		Go:          fakeGo,
	})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}

	commands := strings.Join(runner.commands(), "\n")
	// "(devel)" is what a checkout stamps and no proxy can resolve it, so a
	// replaced module must never be fetched by version.
	if strings.Contains(commands, "go get "+engineModule) {
		t.Errorf("commands =\n%s\nwant the replaced engine to be edited in, not fetched", commands)
	}
	want := "go mod edit -require=" + engineModule + "@v0.0.0 -replace=" + engineModule + "=" + local
	if !strings.Contains(commands, want) {
		t.Errorf("commands =\n%s\nwant %q", commands, want)
	}
	assertNoScratchModule(t, scratchParent)
}

// The message is the whole trade stated out loud: an external plugin needs no
// toolchain, and this is what compiling one in costs.
func TestBuildWithoutAToolchainSaysWhatItNeedsAndWhy(t *testing.T) {
	scratchParent := t.TempDir()
	t.Setenv("PATH", t.TempDir())

	_, err := Build(context.Background(), Request{
		Coordinates: parseAll(t, "github.com/jdoe/lore-linear@v0.3.1"),
		TempDir:     scratchParent,
		Runner:      &fakeRunner{},
	})
	if err == nil {
		t.Fatal("Build() succeeded with no Go toolchain on PATH")
	}
	if internalerror.KindOf(err) != internalerror.KindPrecondition {
		t.Errorf("kind = %v, want precondition", internalerror.KindOf(err))
	}
	for _, want := range []string{"Go toolchain", "compile-time type safety", "https://go.dev/dl/", "lore plugin install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	assertNoScratchModule(t, scratchParent)
}

func TestBuildNeedsAtLeastOnePlugin(t *testing.T) {
	_, err := Build(context.Background(), Request{Runner: &fakeRunner{}, Go: fakeGo})
	if err == nil {
		t.Fatal("Build() built a binary with nothing added to it")
	}
	if internalerror.KindOf(err) != internalerror.KindBadRequest {
		t.Errorf("kind = %v, want bad request", internalerror.KindOf(err))
	}
}

// The slow proof: a real toolchain, the real engine and a real plugin module,
// compiled and then asked what it contains. Everything is local — the engine and
// the plugin are replace directives and the proxy is off — so it needs no
// network, only patience.
func TestBuildProducesARunnableBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("compiling the engine takes about a minute")
	}

	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOSUMDB", "off")

	scratchParent := t.TempDir()
	output := filepath.Join(t.TempDir(), "lore-custom")

	result, err := Build(context.Background(), Request{
		Coordinates: []Coordinate{{Module: "example.com/loreconform", Version: "v0.1.0", Package: "conform"}},
		Output:      output,
		Replace: map[string]string{
			engineModule:              repoRoot(t),
			"example.com/loreconform": writeFakePlugin(t),
		},
		TempDir: scratchParent,
	})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if _, err := os.Stat(result.Output); err != nil {
		t.Fatalf("the binary lore build reported does not exist: %v", err)
	}
	if !strings.Contains(result.Plugins, "conform-fake") {
		t.Errorf("plugin list =\n%s\nwant the compiled-in plugin listed", result.Plugins)
	}
	if !strings.Contains(result.Plugins, "github") {
		t.Errorf("plugin list =\n%s\nwant the official set alongside it", result.Plugins)
	}
	assertNoScratchModule(t, scratchParent)
}

// writeFakePlugin writes the smallest module that satisfies the SDK's source
// contract, which is all a build has to prove it can link.
func writeFakePlugin(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/loreconform\n\ngo 1.27\n\nrequire " + engineModule + " v0.0.0\n",
		"plugin.go": `package conform

import (
	"context"
	"iter"

	"` + engineModule + `/sdk"
)

// Plugin is the constructor every lore plugin exposes.
func Plugin() lore.Plugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       "conform-fake",
		Summary:    "a plugin a test compiled in",
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
	}
}

func (plugin) NewSource(lore.SourceConfig) (lore.Connector, error) { return connector{}, nil }

type connector struct{}

func (connector) Name() string { return "conform-fake" }

func (connector) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(func(lore.Batch, error) bool) {}
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file, so cannot locate the engine to build against")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// The scratch module carries generated code and a stranger's dependencies; a
// build that leaves one behind leaves both in the temporary directory.
func assertNoScratchModule(t *testing.T, parent string) {
	t.Helper()

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read %s: %v", parent, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s still holds %v, want the scratch module removed", parent, names)
	}
}
