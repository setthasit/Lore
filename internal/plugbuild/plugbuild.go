// Package plugbuild compiles a custom lore binary. Go cannot load code
// dynamically, so a compiled third-party plugin always means a new binary: this
// package generates the composition root cmd/lore/main.go already is, with the
// named plugin modules appended to the official set, builds it in a scratch
// module and throws that module away.
//
// It also serves `lore plugin search`, which reads a JSON index over HTTP. The
// two live together because both are the ecosystem's edge: one produces a binary
// with a stranger's code compiled in, the other is how that stranger is found.
package plugbuild

import (
	"cmp"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

// DefaultOutput matches the stock binary's name, so a custom build is a drop-in
// replacement for the one it was built from.
const DefaultOutput = "lore"

// scratchModule is the main module path of the generated build. It never leaves
// the temporary directory, so the name only has to be a legal module path.
const scratchModule = "lorecustom"

// develVersion is what the go command stamps into a binary built from a
// checkout. It is not a version query anything can resolve.
const develVersion = "(devel)"

// Runner runs one external program and returns its combined output. It is an
// interface so a test can assert the exact command sequence without a
// toolchain, a network or a minute of compilation.
type Runner interface {
	Run(ctx context.Context, dir, program string, args ...string) (string, error)
}

// Request describes the binary to build.
type Request struct {
	// Coordinates are the plugin modules to compile in. At least one is
	// required: building the stock plugin set is what `go build ./cmd/lore` is.
	Coordinates []Coordinate

	// Output is the path of the binary to write; empty means DefaultOutput.
	Output string

	// Engine is the version query for the engine module. Empty means the
	// version of the running binary.
	Engine string

	// Replace maps a module path to a local directory, written into the scratch
	// module as a replace directive. Without it a build from an unpublished
	// checkout has no engine version to fetch, which is exactly the situation of
	// anyone developing a plugin against an unreleased engine.
	Replace map[string]string

	// TempDir is the parent of the scratch module; empty means the system
	// temporary directory.
	TempDir string

	// Progress receives one line per step. A custom build compiles the whole
	// engine, so silence for a minute would read as a hang.
	Progress io.Writer

	// Runner runs the go command and the produced binary; nil means the real
	// one. Go is the path to the go command; empty means the one on PATH.
	Runner Runner
	Go     string
}

// Result reports what was built.
type Result struct {
	// Output is the absolute path of the binary that was written.
	Output string

	// Engine is the engine version query the build resolved against, and Added
	// the plugins compiled in on top of the official set, in the order the
	// generated composition root registers them.
	Engine string
	Added  []Coordinate

	// Plugins is the verbatim `lore plugin list` output of the binary that was
	// just built. The plugin set is read back from the artifact rather than
	// predicted, because only the artifact can be right about itself.
	Plugins string
}

// Build generates, fetches, compiles and then asks the produced binary what it
// contains. The scratch module is removed however the build ends.
func Build(ctx context.Context, req Request) (Result, error) {
	goBin := req.Go
	if goBin == "" {
		found, err := FindGo()
		if err != nil {
			return Result{}, err
		}
		goBin = found
	}

	if len(req.Coordinates) == 0 {
		return Result{}, internalerror.NewBadRequestError(
			"lore build needs at least one --with <module>@<version>: a binary with only the official plugins is the one you already have", nil)
	}
	coords := sortedByModule(req.Coordinates)
	source, err := Render(coords)
	if err != nil {
		return Result{}, err
	}

	output, err := filepath.Abs(cmp.Or(req.Output, DefaultOutput))
	if err != nil {
		return Result{}, internalerror.NewInternalError("lore build cannot resolve the output path", err)
	}

	dir, err := os.MkdirTemp(req.TempDir, "lore-build-")
	if err != nil {
		return Result{}, internalerror.NewInternalError("lore build cannot create a scratch module", err)
	}
	// The scratch module holds nothing worth keeping, and a failed build that
	// leaves one behind leaves generated code with somebody's plugin in it
	// sitting in the temporary directory.
	defer func() { _ = os.RemoveAll(dir) }()

	if err := os.WriteFile(filepath.Join(dir, generatedFile), source, 0o600); err != nil {
		return Result{}, internalerror.NewInternalError("lore build cannot write the generated composition root", err)
	}

	engine := cmp.Or(req.Engine, engineVersion())
	runner := cmp.Or(req.Runner, Runner(execRunner{}))
	progress := progressWriter(req.Progress)

	printStep(progress, "generated a composition root for "+strings.Join(names(coords), ", "))

	if out, err := runner.Run(ctx, dir, goBin, "mod", "init", scratchModule); err != nil {
		return Result{}, toolchainError("lore build cannot initialise the scratch module", out, err)
	}

	requirements := append([]Coordinate{{Module: engineModule, Version: engine}}, coords...)
	for _, want := range requirements {
		if local, ok := req.Replace[want.Module]; ok {
			printStep(progress, "using "+want.Module+" from "+local)
			if out, err := runner.Run(ctx, dir, goBin, "mod", "edit",
				"-require="+want.Module+"@"+replacedVersion(want.Version),
				"-replace="+want.Module+"="+local); err != nil {
				return Result{}, toolchainError("lore build cannot point the scratch module at "+local, out, err)
			}
			continue
		}

		printStep(progress, "fetching "+want.Query())
		if out, err := runner.Run(ctx, dir, goBin, "get", want.Query()); err != nil {
			return Result{}, toolchainError("lore build cannot fetch "+want.Query(), out, err)
		}
	}

	// The requirements are complete after the fetches above — every module the
	// generated root imports was named explicitly — but their checksums are not,
	// and a build refuses to compile a module it has no sum for. Download rather
	// than tidy, because tidy would also resolve the test dependencies of every
	// dependency, which no binary needs.
	if out, err := runner.Run(ctx, dir, goBin, "mod", "download"); err != nil {
		return Result{}, toolchainError("lore build cannot download the scratch module's dependencies", out, err)
	}

	printStep(progress, "compiling "+output+" — this builds the engine and every plugin in it")
	if out, err := runner.Run(ctx, dir, goBin, "build", "-o", output, "."); err != nil {
		return Result{}, toolchainError("lore build failed to compile: a plugin named here may not implement "+
			"api version "+strconv.Itoa(lore.APIVersion)+" of the SDK this engine speaks", out, err)
	}

	// Registration validates every manifest against the interfaces it claims,
	// so listing the plugins is also the cheapest proof that the artifact runs.
	listing, err := runner.Run(ctx, "", output, "plugin", "list")
	if err != nil {
		return Result{}, toolchainError("lore build produced "+output+" but it does not run: its plugin set failed to register", listing, err)
	}

	return Result{Output: output, Engine: engine, Added: coords, Plugins: listing}, nil
}

// FindGo locates the Go toolchain. Its absence is reported as the trade it is:
// an external plugin needs nothing installed, and compiling one in is what buys
// in-process calls and compile-time type safety.
func FindGo() (string, error) {
	path, err := exec.LookPath("go")
	if err != nil {
		return "", internalerror.NewPreconditionError(
			"lore build needs a Go toolchain on PATH and found none — compiling a plugin in is what buys "+
				"in-process calls and compile-time type safety, so it needs a compiler: install Go from "+
				"https://go.dev/dl/, or run the plugin out of process with `lore plugin install` instead", err)
	}
	return path, nil
}

// execRunner is the real runner. The environment is inherited: the go command
// needs the user's own GOPATH, GOPROXY, GOMODCACHE and credentials, and this is
// the user's toolchain building the user's code, not a plugin being launched.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir, program string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, program, args...) //nolint:gosec // the program is the go command or the binary just built
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// engineVersion is the version of the running binary, which is the engine a
// custom build should match. A binary built from a checkout reports "(devel)",
// which no proxy can resolve, so the build asks for the newest release instead;
// Request.Replace is how a checkout builds against itself.
func engineVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == develVersion {
		return "latest"
	}
	return info.Main.Version
}

// A replaced module still needs a version on its require line, and the version
// is never used to fetch anything, so a checkout's "(devel)" becomes the zero
// version rather than an error.
func replacedVersion(version string) string {
	if version == "" || version == develVersion || version == "latest" {
		return "v0.0.0"
	}
	return version
}

func toolchainError(message, output string, cause error) error {
	if trimmed := strings.TrimSpace(output); trimmed != "" {
		message += ":\n" + trimmed
	}
	return internalerror.NewPreconditionError(message, cause)
}

func progressWriter(w io.Writer) io.Writer {
	return cmp.Or(w, io.Discard)
}

func printStep(w io.Writer, line string) {
	_, _ = io.WriteString(w, line+"\n")
}

func names(coords []Coordinate) []string {
	out := make([]string, 0, len(coords))
	for _, c := range coords {
		out = append(out, c.String())
	}
	return out
}
