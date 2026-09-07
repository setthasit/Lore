package plugindist

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// scratchWorkspace roots the plugin cache in a temporary directory and seeds a
// configuration beside a temporary lockfile, so no test writes to a real home.
func scratchWorkspace(t *testing.T, body string) string {
	t.Helper()

	t.Setenv(RootEnv, t.TempDir())
	path := filepath.Join(t.TempDir(), "lore.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed a configuration: %v", err)
	}
	return path
}

func openScratch(t *testing.T, path string) *Workspace {
	t.Helper()

	workspace, err := Open(path)
	if err != nil {
		t.Fatalf("open the workspace: %v", err)
	}
	return workspace
}

func TestInstallDeclaresAnUndeclaredCoordinateAtTheResolvedVersion(t *testing.T) {
	path := scratchWorkspace(t, "workspace: myproject\n")
	workspace := openScratch(t, path)

	coord, err := Resolve(".", config.PluginDecl{Name: "linear", From: "github.com/jdoe/lore-linear@v0.4.2"})
	if err != nil {
		t.Fatalf("resolve the published coordinate: %v", err)
	}
	platform := workspace.store.platform
	fake := newFakeGitHub(t, "jdoe", "lore-linear")
	fake.publish("v0.4.2", map[string][]byte{
		coord.assetName(platform): archiveWith(t, coord.binaryName(platform), []byte(stubBinary)),
	})
	workspace.installer = fake.installer(workspace.store)

	results, err := workspace.Install(context.Background(), []string{"github.com/jdoe/lore-linear@latest"}, func() {})
	if err != nil {
		t.Fatalf("install a coordinate at @latest: %v", err)
	}
	if len(results) != 1 || results[0].Version != "v0.4.2" {
		t.Fatalf("results = %+v, want one install of v0.4.2", results)
	}

	const want = "workspace: myproject\nplugins:\n  - name: linear\n    from: github.com/jdoe/lore-linear@v0.4.2\n"
	if after := readFile(t, path); after != want {
		t.Errorf("configuration =\n%s\nwant\n%s", after, want)
	}
}

// @latest asks for the newest release, which only a repository coordinate has.
// Install refuses the others in the same words update already refuses them in,
// rather than gluing "@latest" onto a filename and fetching it.
func TestInstallAtLatestRefusesACoordinateNoArgumentCanRepoint(t *testing.T) {
	cases := []struct {
		name      string
		from      string
		updateArg string
	}{
		{name: "url", from: "https://artifacts.example.com/lore/linear/v2.0.1.tar.gz", updateArg: "linear@v2.1.0"},
		{name: "local", from: "./bin/lore-linear", updateArg: "linear@latest"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := scratchWorkspace(t, "workspace: myproject\n\nplugins:\n  - name: linear\n    from: "+tc.from+"\n")
			workspace := openScratch(t, path)

			_, installErr := workspace.Install(context.Background(), []string{"linear@latest"}, func() {
				t.Error("the trust notice fired for a refused install")
			})
			if installErr == nil {
				t.Fatal("install pinned a coordinate whose version is not an argument")
			}
			if !internalerror.IsBadRequest(installErr) {
				t.Errorf("kind = %v, want %v", internalerror.KindOf(installErr), internalerror.KindBadRequest)
			}

			_, updateErr := openScratch(t, path).Update(context.Background(), tc.updateArg, func() {
				t.Error("the trust notice fired for a refused update")
			})
			if updateErr == nil || updateErr.Error() != installErr.Error() {
				t.Errorf("install refused with %q, update with %v — they must read alike", installErr, updateErr)
			}
			if after := readFile(t, path); !strings.Contains(after, tc.from) {
				t.Errorf("configuration =\n%s\nwant the declaration untouched", after)
			}
		})
	}
}

// config does not reject two plugins: entries sharing a name, so each row of
// `lore plugin list` has to be answered from its own declaration.
func TestInstalledAnswersEachDeclarationFromItsOwnCoordinate(t *testing.T) {
	path := scratchWorkspace(t, "workspace: myproject\n\nplugins:\n"+
		"  - name: linear\n    from: ./lore-linear\n"+
		"  - name: linear\n    from: ./lore-missing\n")
	present := filepath.Join(filepath.Dir(path), "lore-linear")
	if err := os.WriteFile(present, []byte(stubBinary), 0o755); err != nil {
		t.Fatalf("seed a local plugin binary: %v", err)
	}

	workspace := openScratch(t, path)
	decls := workspace.Plugins()
	if len(decls) != 2 {
		t.Fatalf("declarations = %+v, want the two the file holds", decls)
	}

	binary, err := workspace.Installed(decls[0])
	if err != nil || binary != present {
		t.Fatalf("binary = %q, err = %v, want %q", binary, err, present)
	}

	absent, err := workspace.Installed(decls[1])
	if err != nil {
		t.Fatalf("the second declaration of the same name: %v", err)
	}
	if absent != "" {
		t.Errorf("binary = %q, want the empty path of a declaration nothing is installed for", absent)
	}
}

func TestPinEditsSpliceOnceForEachPinnedCoordinate(t *testing.T) {
	t.Parallel()

	pinned := Coordinate{Name: "linear", From: "github.com/jdoe/lore-linear@v0.4.2"}
	requested := func(from string) Request {
		return Request{Coordinate: Coordinate{Name: "linear", From: from}}
	}

	cases := []struct {
		name    string
		request Request
		declare string
		want    []configEdit
	}{{
		name:    "an undeclared coordinate is declared at the pinned version",
		request: requested("github.com/jdoe/lore-linear@latest"),
		declare: "linear",
		want:    []configEdit{{name: "linear", from: pinned.From, kind: editDeclare}},
	}, {
		name:    "a declared coordinate that moved is rewritten",
		request: requested("github.com/jdoe/lore-linear@latest"),
		want:    []configEdit{{name: "linear", from: pinned.From, kind: editPin}},
	}, {
		name:    "a coordinate that was already exact is left alone",
		request: requested(pinned.From),
		want:    nil,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			edits := pinEdits([]Request{tc.request}, []Coordinate{pinned}, tc.declare)
			if len(edits) != len(tc.want) {
				t.Fatalf("edits = %+v, want %+v", edits, tc.want)
			}
			for i, edit := range edits {
				if edit != tc.want[i] {
					t.Errorf("edit %d = %+v, want %+v", i, edit, tc.want[i])
				}
			}
		})
	}
}

// Remove rewrites the configuration, then the lockfile, then deletes the cache.
// The cache is the only one of the three a later install rebuilds, so a refused
// lockfile write must not find it already thrown away.
func TestRemoveKeepsTheCacheWhenTheLockfileCannotBeWritten(t *testing.T) {
	path := scratchWorkspace(t, "workspace: myproject\n\nplugins:\n  - name: linear\n"+
		"    from: github.com/jdoe/lore-linear@v0.3.1\n")
	dir := filepath.Dir(path)

	locked := &Lock{}
	locked.Set("linear", "v0.3.1", "github.com/jdoe/lore-linear@v0.3.1", hostPlatform(),
		LockArtifact{URL: "https://artifacts.invalid/lore-linear.tar.gz", Digest: "sha256:fake"})
	if err := locked.Save(dir); err != nil {
		t.Fatalf("seed a lockfile: %v", err)
	}

	workspace := openScratch(t, path)
	cached := seedCache(t, workspace.store, "linear", "v0.3.1")

	// An atomic rename cannot replace a directory, so the lockfile write fails
	// once the configuration has already been rewritten.
	if err := os.Remove(lockPath(dir)); err != nil {
		t.Fatalf("clear the lockfile: %v", err)
	}
	if err := os.Mkdir(lockPath(dir), 0o755); err != nil {
		t.Fatalf("stage an unwritable lockfile: %v", err)
	}

	switch _, err := workspace.Remove("linear"); {
	case err == nil:
		t.Fatal("a lockfile that cannot be written was reported as removed")
	case !internalerror.IsInternal(err):
		t.Errorf("kind = %v, want %v", internalerror.KindOf(err), internalerror.KindInternal)
	}

	if _, err := os.Stat(cached); err != nil {
		t.Errorf("the plugin cache at %s is gone: %v", cached, err)
	}
	if after := readFile(t, path); strings.Contains(after, "linear") {
		t.Errorf("configuration =\n%s\nwant the declaration already dropped", after)
	}
}

func TestConfigEditRefusesAnUnhandledKind(t *testing.T) {
	t.Parallel()

	_, err := configEdit{name: "linear", kind: editKind(99)}.apply(
		"workspace: myproject\n\nplugins:\n  - name: linear\n    from: github.com/jdoe/lore-linear@v0.3.1\n")
	if err == nil {
		t.Fatal("an unhandled edit kind was applied")
	}
	if !internalerror.IsInternal(err) {
		t.Errorf("kind = %v, want %v", internalerror.KindOf(err), internalerror.KindInternal)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error = %q, want it to name the unhandled kind", err)
	}
}

func seedCache(t *testing.T, store *Store, name, version string) string {
	t.Helper()

	dir, err := store.Dir(name, version)
	if err != nil {
		t.Fatalf("resolve the cache directory: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seed the plugin cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(stubBinary), 0o755); err != nil {
		t.Fatalf("seed the cached binary: %v", err)
	}
	return dir
}
