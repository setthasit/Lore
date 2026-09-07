package plugindist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const wantLock = `# lore.lock — generated; written by ` + "`lore plugin install|update`" + `
version: 1
plugins:
  acme-crm:
    version: v2.0.1
    from: https://artifacts.acme.internal/lore/acme-crm/v2.0.1.tar.gz
    artifacts:
      darwin/arm64:
        url: https://artifacts.acme.internal/lore/acme-crm/v2.0.1.tar.gz
        digest: sha256:5c7d90ab
  linear:
    version: v0.3.1
    from: github.com/jdoe/lore-linear@v0.3.1
    artifacts:
      darwin/arm64:
        url: https://github.com/jdoe/lore-linear/releases/download/v0.3.1/lore-linear_0.3.1_darwin_arm64.tar.gz
        digest: sha256:9f2b41c0
      linux/amd64:
        url: https://github.com/jdoe/lore-linear/releases/download/v0.3.1/lore-linear_0.3.1_linux_amd64.tar.gz
        digest: sha256:1a08be77
`

func TestLockRoundTripsExactBytes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lock, err := LoadLock(dir)
	if err != nil {
		t.Fatalf("load an absent lock: %v", err)
	}

	darwin, linux := Platform{OS: "darwin", Arch: "arm64"}, Platform{OS: "linux", Arch: "amd64"}
	const release = "https://github.com/jdoe/lore-linear/releases/download/v0.3.1/"
	// The plugins are set out of alphabetical order: the file must sort, not follow install order.
	lock.Set("linear", "v0.3.1", "github.com/jdoe/lore-linear@v0.3.1", linux,
		LockArtifact{URL: release + "lore-linear_0.3.1_linux_amd64.tar.gz", Digest: "sha256:1a08be77"})
	lock.Set("acme-crm", "v2.0.1", "https://artifacts.acme.internal/lore/acme-crm/v2.0.1.tar.gz", darwin,
		LockArtifact{
			URL:    "https://artifacts.acme.internal/lore/acme-crm/v2.0.1.tar.gz",
			Digest: "sha256:5c7d90ab",
		})
	lock.Set("linear", "v0.3.1", "github.com/jdoe/lore-linear@v0.3.1", darwin,
		LockArtifact{URL: release + "lore-linear_0.3.1_darwin_arm64.tar.gz", Digest: "sha256:9f2b41c0"})

	if err := lock.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := readFile(t, lockPath(dir)); got != wantLock {
		t.Fatalf("lore.lock =\n%s\nwant\n%s", got, wantLock)
	}

	reloaded, err := LoadLock(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	artifact, locked := reloaded.Artifact("linear", darwin)
	if !locked || artifact.Digest != "sha256:9f2b41c0" {
		t.Fatalf("linear on darwin/arm64 = %+v, locked = %v", artifact, locked)
	}
	if _, locked := reloaded.Artifact("linear", Platform{OS: "windows", Arch: "amd64"}); locked {
		t.Fatal("a platform nobody installed for is locked")
	}
}

func TestLockAbsentFileIsEmptyNotAnError(t *testing.T) {
	t.Parallel()

	lock, err := LoadLock(t.TempDir())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(lock.Plugins) != 0 {
		t.Fatalf("plugins = %+v, want none", lock.Plugins)
	}
	if _, found := lock.Entry("linear"); found {
		t.Fatal("an empty lock holds an entry")
	}
}

func TestLockRefusesAFileFromTheFuture(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(lockPath(dir), []byte("version: 2\nplugins: {}\n"), 0o600); err != nil {
		t.Fatalf("seed a lockfile: %v", err)
	}

	_, err := LoadLock(dir)
	if err == nil {
		t.Fatal("loading a version 2 lockfile succeeded, want a refusal")
	}
	if !internalerror.IsPrecondition(err) {
		t.Fatalf("kind = %v, want precondition", internalerror.KindOf(err))
	}
	if !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("error %q does not name the version it read", err)
	}
}

func TestLockNewVersionDropsStaleDigests(t *testing.T) {
	t.Parallel()

	darwin, linux := Platform{OS: "darwin", Arch: "arm64"}, Platform{OS: "linux", Arch: "amd64"}
	lock := &Lock{}
	lock.Set("linear", "v0.3.1", "github.com/jdoe/lore-linear@v0.3.1", linux,
		LockArtifact{URL: "https://example.test/a.tar.gz", Digest: "sha256:aaaa"})
	lock.Set("linear", "v0.4.0", "github.com/jdoe/lore-linear@v0.4.0", darwin,
		LockArtifact{URL: "https://example.test/b.tar.gz", Digest: "sha256:bbbb"})

	if _, locked := lock.Artifact("linear", linux); locked {
		t.Fatal("a v0.3.1 digest survived the move to v0.4.0")
	}
	entry, _ := lock.Entry("linear")
	if entry.Version != "v0.4.0" {
		t.Fatalf("version = %q, want v0.4.0", entry.Version)
	}
}

func TestLockSaveKeepsTheModeOfAnExistingLockfile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, LockFileName)
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o664); err != nil {
		t.Fatalf("seed a lockfile: %v", err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatalf("make the lockfile group-writable past the umask: %v", err)
	}

	lock := &Lock{}
	if err := lock.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o664 {
		t.Errorf("mode = %v, want %v", got, os.FileMode(0o664))
	}
}

func TestLockSaveWritesANewLockfileWorldReadable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lock := &Lock{}
	if err := lock.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, LockFileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %v, want %v", got, os.FileMode(0o644))
	}
}
