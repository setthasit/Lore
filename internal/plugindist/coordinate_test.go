package plugindist

import (
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
)

func TestCoordinateDispatchesByShape(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		from    string
		origin  Origin
		version string
	}{
		{name: "relative path", from: "./bin/lore-scratch", origin: OriginLocal},
		{name: "parent path", from: "../plugins/lore-scratch", origin: OriginLocal},
		{name: "absolute path", from: "/opt/lore/lore-scratch", origin: OriginLocal},
		{name: "release", from: "github.com/jdoe/lore-linear@v0.3.1", origin: OriginGitHub, version: "v0.3.1"},
		{name: "prerelease", from: "github.com/jdoe/lore-linear@v1.0.0-rc.1", origin: OriginGitHub, version: "v1.0.0-rc.1"},
		{
			name:    "artifact url",
			from:    "https://artifacts.acme.internal/lore/acme-crm/v2.0.1.tar.gz",
			origin:  OriginURL,
			version: "v2.0.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			coord, err := Resolve(".", config.PluginDecl{Name: "scratch", From: tc.from})
			if err != nil {
				t.Fatalf("resolve %s: %v", tc.from, err)
			}
			if coord.Origin != tc.origin {
				t.Fatalf("origin = %q, want %q", coord.Origin, tc.origin)
			}
			if coord.Version != tc.version {
				t.Fatalf("version = %q, want %q", coord.Version, tc.version)
			}
			if coord.From != tc.from {
				t.Fatalf("from = %q, want %q", coord.From, tc.from)
			}
		})
	}
}

// A floating version in lore.yaml is the failure this whole file exists to
// prevent: two machines would resolve it differently and run different code
// against one index.
func TestCoordinateRefusesFloatingVersionInConfig(t *testing.T) {
	t.Parallel()

	_, err := Resolve(".", config.PluginDecl{Name: "linear", From: "github.com/jdoe/lore-linear@latest"})
	if err == nil {
		t.Fatal("resolving @latest from a configuration succeeded, want a refusal")
	}
	if !internalerror.IsBadRequest(err) {
		t.Fatalf("kind = %v, want bad request", internalerror.KindOf(err))
	}
	for _, want := range []string{"plugins[linear]", "@latest", "lore plugin install linear@latest"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	// The same coordinate is legal as an install argument, which is the only
	// place that can pin it back into the file.
	if _, err := ResolveInstall(".", "linear", "github.com/jdoe/lore-linear@latest"); err != nil {
		t.Fatalf("resolving @latest as an install argument: %v", err)
	}
}

func TestCoordinateRefusesUnpinnedAndMalformed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		from string
		want string
	}{
		{name: "no version", from: "github.com/jdoe/lore-linear", want: "pins no version"},
		{name: "branch", from: "github.com/jdoe/lore-linear@main", want: "is not an exact version"},
		{name: "no repository", from: "github.com/jdoe@v0.3.1", want: "names no repository"},
		{name: "plaintext", from: "http://artifacts.example.com/x/v1.0.0.tar.gz", want: "plaintext HTTP"},
		{name: "bare token", from: "lore-linear", want: "is not a coordinate"},
		{name: "unversioned url", from: "https://artifacts.example.com/", want: "ends in no version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Resolve(".", config.PluginDecl{Name: "linear", From: tc.from})
			if err == nil {
				t.Fatalf("resolving %s succeeded, want a refusal", tc.from)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The asset name is constructed, never guessed at: that is what lets a release
// which ignores the convention fail with the name that was looked for.
func TestCoordinateAssetNameFollowsConvention(t *testing.T) {
	t.Parallel()

	coord, err := Resolve(".", config.PluginDecl{Name: "linear", From: "github.com/jdoe/lore-linear@v0.3.1"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	const want = "lore-linear_0.3.1_darwin_arm64.tar.gz"
	if got := coord.AssetName(Platform{OS: "darwin", Arch: "arm64"}); got != want {
		t.Fatalf("asset = %q, want %q", got, want)
	}
	if got := coord.binaryName(Platform{OS: "windows", Arch: "amd64"}); got != "lore-linear.exe" {
		t.Fatalf("binary = %q, want lore-linear.exe", got)
	}
}

// A local plugin is the development escape hatch, and the warning is the whole
// price: no lock entry, no digest, no way to know what changed.
func TestCoordinateLocalWarnsAndIsNotLocked(t *testing.T) {
	t.Parallel()

	coord, err := Resolve(".", config.PluginDecl{Name: "scratch", From: "./bin/lore-scratch"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if coord.Remote() {
		t.Fatal("a local coordinate reports itself remote")
	}
	for _, want := range []string{"plugins[scratch]", "unpinned", "development only"} {
		if !strings.Contains(coord.Warning(), want) {
			t.Fatalf("warning %q does not mention %q", coord.Warning(), want)
		}
	}

	remote, err := Resolve(".", config.PluginDecl{Name: "linear", From: "github.com/jdoe/lore-linear@v0.3.1"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if remote.Warning() != "" {
		t.Fatalf("a pinned coordinate warns: %q", remote.Warning())
	}
}
