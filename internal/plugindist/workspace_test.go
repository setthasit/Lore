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
	publishRelease(t, workspace, coord)

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

	install, err := workspace.Installed(decls[0])
	if err != nil || install.Fault != nil || install.Binary != present {
		t.Fatalf("install = %+v, err = %v, want %q", install, err, present)
	}

	absent, err := workspace.Installed(decls[1])
	if err != nil {
		t.Fatalf("the second declaration of the same name: %v", err)
	}
	if absent.Binary != "" || absent.Fault != nil {
		t.Errorf("install = %+v, want the empty answer of a declaration nothing is installed for", absent)
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
// Only the cache is rebuilt by re-running install, so it goes last.
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

	// An atomic rename cannot replace a directory, so a directory at the lock path makes the write fail.
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

const expandedLocal = "workspace: myproject\n\nplugins:\n  - name: linear\n    from: ${env:LORE_PLUGIN_DIR}/lore-linear\n"

func stagePluginDir(t *testing.T, binaries ...string) string {
	t.Helper()

	dir := t.TempDir()
	for _, binary := range binaries {
		if err := os.WriteFile(filepath.Join(dir, binary), []byte(stubBinary), 0o755); err != nil {
			t.Fatalf("seed a local plugin binary: %v", err)
		}
	}
	t.Setenv("LORE_PLUGIN_DIR", dir)
	return dir
}

func TestAnExpandedFromInstallsListsAndVerifiesTheExpandedPath(t *testing.T) {
	path := scratchWorkspace(t, expandedLocal)
	binary := filepath.Join(stagePluginDir(t, "lore-linear"), "lore-linear")
	workspace := openScratch(t, path)

	results, err := workspace.Install(context.Background(), nil, func() {})
	if err != nil || len(results) != 1 || results[0].Binary != binary {
		t.Fatalf("results = %+v, err = %v, want one install of %s", results, err, binary)
	}
	if install, err := workspace.Installed(workspace.Plugins()[0]); err != nil || install.Binary != binary {
		t.Errorf("install = %+v, err = %v, want %s", install, err, binary)
	}
	if report, err := workspace.Verify("linear"); err != nil || report.Binary != binary {
		t.Errorf("report = %+v, err = %v, want %s", report, err, binary)
	}
	if after := readFile(t, path); after != expandedLocal {
		t.Errorf("configuration =\n%s\nwant it untouched", after)
	}
}

func TestAnUnsetVariableRefusesToOpenTheWorkspace(t *testing.T) {
	path := scratchWorkspace(t, expandedLocal)
	unsetEnv(t, "LORE_PLUGIN_DIR")

	_, err := Open(path)
	const want = "plugins[0].from expands LORE_PLUGIN_DIR, but LORE_PLUGIN_DIR is not set"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if !internalerror.IsBadRequest(err) {
		t.Errorf("kind = %v, want %v", internalerror.KindOf(err), internalerror.KindBadRequest)
	}
}

func TestAPinRefusesToOverwriteAnExpandedFrom(t *testing.T) {
	cases := []struct {
		name     string
		from     string
		variable string
		value    string
	}{
		{name: "whole value", from: "${env:LORE_LINEAR_FROM}", variable: "LORE_LINEAR_FROM",
			value: "github.com/jdoe/lore-linear@v0.3.1"},
		{name: "mid value", from: "github.com/${env:LORE_LINEAR_OWNER}/lore-linear@v0.3.1",
			variable: "LORE_LINEAR_OWNER", value: "jdoe"},
	}
	const want = "cannot pin plugins[linear]: plugins[0].from is written with ${env:VAR}, which a pin never" +
		" overwrites — point the variable at the new coordinate instead"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "workspace: myproject\n\nplugins:\n  - name: linear\n    from: " + tc.from + "\n"
			path := scratchWorkspace(t, body)
			t.Setenv(tc.variable, tc.value)
			latest, err := Resolve(".", config.PluginDecl{Name: "linear", From: "github.com/jdoe/lore-linear@v0.4.2"})
			if err != nil {
				t.Fatalf("resolve the published coordinate: %v", err)
			}

			updating := openScratch(t, path)
			publishRelease(t, updating, latest)
			_, err = updating.Update(context.Background(), "linear@v0.4.2", func() {
				t.Error("the trust notice fired for a refused update")
			})
			if err == nil || err.Error() != want || !internalerror.IsPrecondition(err) {
				t.Errorf("update error = %v, want the precondition %q", err, want)
			}

			installing := openScratch(t, path)
			publishRelease(t, installing, latest)
			if _, err := installing.Install(context.Background(), []string{"linear@latest"}, func() {}); err == nil ||
				err.Error() != want {
				t.Errorf("install error = %v, want %q", err, want)
			}

			if after := readFile(t, path); after != body {
				t.Errorf("configuration =\n%s\nwant it untouched", after)
			}
			if _, err := os.Stat(lockPath(filepath.Dir(path))); !os.IsNotExist(err) {
				t.Errorf("a refused pin wrote a lockfile: %v", err)
			}
		})
	}
}

func TestRemoveRefusesAPluginAnExpandedUseStillNames(t *testing.T) {
	const body = "workspace: myproject\n\nplugins:\n  - name: ${env:LORE_LINEAR_NAME}\n    from: ./lore-linear\n" +
		"\nsources:\n  - use: ${env:LORE_LINEAR_NAME}\n" +
		"\nrepos:\n  - path: /srv/lore\n    use: ${env:LORE_LINEAR_NAME}\n"
	path := scratchWorkspace(t, body)
	t.Setenv("LORE_LINEAR_NAME", "linear")
	workspace := openScratch(t, path)

	if source, found := workspace.SourceUsing("linear"); !found || source.Use != "linear" {
		t.Errorf("source = %+v, found = %v, want the source whose use: expands to linear", source, found)
	}

	_, err := workspace.Remove("linear")
	const want = "plugins[linear] is still used by sources[linear], repos[/srv/lore]" +
		" — remove those first, or the next `lore sync` has nothing to build them from"
	if err == nil || err.Error() != want || !internalerror.IsPrecondition(err) {
		t.Errorf("error = %v, want the precondition %q", err, want)
	}
	if after := readFile(t, path); after != body {
		t.Errorf("configuration =\n%s\nwant it untouched", after)
	}
}

func TestAnInstallMismatchNamesAnExpandedFromInsteadOfItsValue(t *testing.T) {
	path := scratchWorkspace(t, "workspace: myproject\n\nplugins:\n  - name: linear\n    from: ${env:LORE_LINEAR_FROM}\n")
	t.Setenv("LORE_LINEAR_FROM", "https://artifacts.example.com/lore/linear/v2.0.1.tar.gz?sig=fake-signature")

	_, err := openScratch(t, path).Install(context.Background(),
		[]string{"github.com/jdoe/lore-linear@v1.0.0"}, func() {
			t.Error("the trust notice fired for a refused install")
		})
	want := path + " declares linear from plugins[0].from, not github.com/jdoe/lore-linear@v1.0.0" +
		" — edit the declaration, or run: lore plugin update linear"
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

func TestOpenLeavesFieldsNoPluginCommandReadsUnexpanded(t *testing.T) {
	path := scratchWorkspace(t, "workspace: ${env:LORE_UNSET_WORKSPACE}\n\n"+
		"repos:\n  - path: /srv/lore\n    remote: ${env:LORE_UNSET_REMOTE}\n")
	unsetEnv(t, "LORE_UNSET_WORKSPACE", "LORE_UNSET_REMOTE")

	if _, err := Open(path); err != nil {
		t.Errorf("open: %v, want the unset variables of workspace and repos[0].remote left unread", err)
	}
}

func unsetEnv(t *testing.T, names ...string) {
	t.Helper()

	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

func TestEditsLeaveExpansionsRawInTheConfiguration(t *testing.T) {
	const scratch = "  - name: ${env:LORE_SCRATCH_NAME}\n    from: ${env:LORE_PLUGIN_DIR}/lore-scratch\n"
	path := scratchWorkspace(t, "workspace: myproject\n\nplugins:\n"+
		"  - name: linear\n    from: github.com/jdoe/lore-linear@v0.3.1\n"+scratch)
	binary := filepath.Join(stagePluginDir(t, "lore-scratch"), "lore-scratch")
	t.Setenv("LORE_SCRATCH_NAME", "scratch")

	workspace := openScratch(t, path)
	coord, err := Resolve(".", config.PluginDecl{Name: "linear", From: "github.com/jdoe/lore-linear@v0.4.2"})
	if err != nil {
		t.Fatalf("resolve the published coordinate: %v", err)
	}
	publishRelease(t, workspace, coord)
	if _, err := workspace.Update(context.Background(), "linear@v0.4.2", func() {}); err != nil {
		t.Fatalf("update linear: %v", err)
	}
	updated := "workspace: myproject\n\nplugins:\n  - name: linear\n    from: github.com/jdoe/lore-linear@v0.4.2\n" + scratch
	if after := readFile(t, path); after != updated {
		t.Fatalf("after update, configuration =\n%s\nwant\n%s", after, updated)
	}

	if _, err := openScratch(t, path).Remove("linear"); err != nil {
		t.Fatalf("remove linear: %v", err)
	}
	if after, want := readFile(t, path), "workspace: myproject\n\nplugins:\n"+scratch; after != want {
		t.Fatalf("after removing linear, configuration =\n%s\nwant\n%s", after, want)
	}

	workspace = openScratch(t, path)
	if report, err := workspace.Verify("scratch"); err != nil || report.Binary != binary {
		t.Fatalf("report = %+v, err = %v, want %s", report, err, binary)
	}
	if _, err := workspace.Remove("scratch"); err != nil {
		t.Fatalf("remove the plugin its expanded name declares: %v", err)
	}
	if after := readFile(t, path); after != "workspace: myproject\n\n" {
		t.Errorf("after removing scratch, configuration =\n%s\nwant no plugins left", after)
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
