package plugindist

import (
	"os"
	"path/filepath"
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
		{name: "home path", from: "~/plugins/lore-scratch", origin: OriginLocal},
		{name: "windows relative path", from: `.\bin\lore-scratch`, origin: OriginLocal},
		{name: "windows parent path", from: `..\plugins\lore-scratch`, origin: OriginLocal},
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
		})
	}
}

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

	if _, err := ResolveInstall(".", config.PluginDecl{Name: "linear", From: "github.com/jdoe/lore-linear@latest"}); err != nil {
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
		{name: "major version only", from: "github.com/jdoe/lore-linear@v1", want: "is not an exact version"},
		{name: "minor version only", from: "github.com/jdoe/lore-linear@v1.2", want: "is not an exact version"},
		{name: "branch", from: "github.com/jdoe/lore-linear@main", want: "is not an exact version"},
		{name: "no repository", from: "github.com/jdoe@v0.3.1", want: "names no repository"},
		{name: "plaintext", from: "http://artifacts.example.com/x/v1.0.0.tar.gz", want: "plaintext HTTP"},
		{name: "bare token", from: "lore-linear", want: "is not a coordinate"},
		{name: "unversioned url", from: "https://artifacts.example.com/", want: "ends in no version"},
		{name: "dot-leading url version", from: "https://artifacts.example.com/lore/linear/.foo.tar.gz", want: "ends in no version"},
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

func TestCoordinateAssetNameFollowsConvention(t *testing.T) {
	t.Parallel()

	coord, err := Resolve(".", config.PluginDecl{Name: "linear", From: "github.com/jdoe/lore-linear@v0.3.1"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	const want = "lore-linear_0.3.1_darwin_arm64.tar.gz"
	if got := coord.assetName(Platform{OS: "darwin", Arch: "arm64"}); got != want {
		t.Fatalf("asset = %q, want %q", got, want)
	}
	if got := coord.binaryName(Platform{OS: "windows", Arch: "amd64"}); got != "lore-linear.exe" {
		t.Fatalf("binary = %q, want lore-linear.exe", got)
	}
}

func TestCoordinateLocalWarnsAndIsNotLocked(t *testing.T) {
	t.Parallel()

	coord, err := Resolve(".", config.PluginDecl{Name: "scratch", From: "./bin/lore-scratch"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
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

func TestCoordinateRefusesANameThatIsNotOnePathComponent(t *testing.T) {
	t.Parallel()

	const from = "github.com/jdoe/lore-linear@v0.3.1"
	for _, name := range []string{"..", "../..", "../pwned", "linear/../..", "a/b", `a\b`, "/etc/lore", ".", "-flag", "two words",
		"Acme_CRM", "acme_crm", "2fa", "linear-", "a--b"} {
		_, err := Resolve(".", config.PluginDecl{Name: name, From: from})
		if err == nil {
			t.Errorf("declaring the name %q resolved, want a refusal", name)
			continue
		}
		if !internalerror.IsBadRequest(err) {
			t.Errorf("name %q: kind = %v, want bad request", name, internalerror.KindOf(err))
		}
	}

	for _, name := range []string{"linear", "jira-acme", "acme-crm", "s3"} {
		if _, err := Resolve(".", config.PluginDecl{Name: name, From: from}); err != nil {
			t.Errorf("name %q was refused: %v", name, err)
		}
	}
}

func TestCoordinateResolvesPubKeyAgainstTheConfigDirectory(t *testing.T) {
	t.Parallel()

	dir, absolute := t.TempDir(), filepath.Join(t.TempDir(), "elsewhere.pub")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("read the home directory: %v", err)
	}

	for _, tc := range []struct {
		name     string
		declared string
		want     string
	}{
		{name: "dot relative", declared: "./keys/jdoe.pub", want: filepath.Join(dir, "keys", "jdoe.pub")},
		{name: "bare relative", declared: "keys/jdoe.pub", want: filepath.Join(dir, "keys", "jdoe.pub")},
		{name: "parent relative", declared: "../jdoe.pub", want: filepath.Join(filepath.Dir(dir), "jdoe.pub")},
		{name: "absolute", declared: absolute, want: absolute},
		{name: "home relative", declared: "~/keys/jdoe.pub", want: filepath.Join(home, "keys", "jdoe.pub")},
		{name: "home itself", declared: "~", want: home},
		{name: "home with a separator", declared: "~" + string(filepath.Separator), want: home},
		{name: "unset means unsigned", declared: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			coord, err := Resolve(dir, config.PluginDecl{
				Name: "linear", From: "github.com/jdoe/lore-linear@v0.3.1", PubKey: tc.declared,
			})
			if err != nil {
				t.Fatalf("resolve pubkey %q: %v", tc.declared, err)
			}
			if coord.PubKey != tc.want {
				t.Fatalf("PubKey = %q, want %q", coord.PubKey, tc.want)
			}
		})
	}
}

func TestCoordinateRefusesABlankPubKey(t *testing.T) {
	t.Parallel()

	for _, declared := range []string{" ", "\t", " \n ", "  \t "} {
		_, err := Resolve(t.TempDir(), config.PluginDecl{
			Name: "linear", From: "github.com/jdoe/lore-linear@v0.3.1", PubKey: declared,
		})
		if err == nil {
			t.Errorf("pubkey %q resolved, want a refusal", declared)
			continue
		}
		if !internalerror.IsBadRequest(err) {
			t.Errorf("pubkey %q: kind = %v, want bad request", declared, internalerror.KindOf(err))
		}
		if !strings.Contains(err.Error(), "plugins[linear]") {
			t.Errorf("error %q does not name the declaration", err)
		}
	}
}

func TestCoordinateKeepsPubKeyAcrossAtVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	coord, err := Resolve(dir, config.PluginDecl{
		Name: "linear", From: "github.com/jdoe/lore-linear@v0.3.1", PubKey: "./keys/jdoe.pub",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	moved, err := coord.AtVersion("v0.4.0")
	if err != nil {
		t.Fatalf("move the coordinate: %v", err)
	}
	if moved.PubKey != coord.PubKey {
		t.Fatalf("PubKey = %q, want %q", moved.PubKey, coord.PubKey)
	}
}
