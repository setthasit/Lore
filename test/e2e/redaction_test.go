package e2e

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/app"
	"github.com/setthasit/Lore/internal/plugexec"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/secrets"
	"github.com/setthasit/Lore/sdk"
)

const (
	leakyPluginName = "e2e-leaky"

	leakyTokenEnv = "LORE_E2E_LEAKY_TOKEN"
	leakyPinEnv   = "LORE_E2E_LEAKY_PIN"

	leakyToken   = "fake-leaky-token-7f3a"
	leakyLongPin = "fake-leaky-pin-5d21"
	leakyShort   = "4821"

	leakyLoggedLine = "opening the leaky source"
	leakyStderrLine = "raw token "
)

var leakyNotice = "lore: secrets shorter than 8 characters are not scrubbed: " +
	leakyPinEnv + " (sources[" + leakyPluginName + "].with.pin_env)\n"

var leakySourceBlock = "sources:\n" +
	"  - use: " + leakyPluginName + "\n" +
	"    with:\n" +
	"      token_env: " + leakyTokenEnv + "\n" +
	"      pin_env: " + leakyPinEnv + "\n"

type leakySourcePlugin struct{}

var _ lore.SourcePlugin = leakySourcePlugin{}

func (leakySourcePlugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       leakyPluginName,
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "compiled-in source that logs its own secrets",
		Secrets: []lore.Secret{
			{Key: "token", ConfigField: "token_env"},
			{Key: "pin", ConfigField: "pin_env"},
		},
	}
}

func (leakySourcePlugin) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	c.Host.Log.Info(leakyLoggedLine, "token", c.Secret("token"), "pin", c.Secret("pin"))
	return emptyConnector(c.Instance), nil
}

type emptyConnector string

func (c emptyConnector) Name() string { return string(c) }

func (emptyConnector) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(func(lore.Batch, error) bool) {}
}

const externalLeakySource = `package main

import (
	"context"
	"fmt"
	"iter"
	"os"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/stdio"
)

func main() {
	if err := stdio.Serve(plugin{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       %q,
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "external source that holds two secrets",
		Secrets: []lore.Secret{
			{Key: "token", ConfigField: "token_env"},
			{Key: "pin", ConfigField: "pin_env"},
		},
	}
}

func (plugin) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	fmt.Fprintln(os.Stderr, %q+c.Secret("token"))
	return empty(c.Instance), nil
}

type empty string

func (e empty) Name() string { return string(e) }

func (empty) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(func(lore.Batch, error) bool) {}
}
`

type loreRun struct {
	exitCode int
	stdout   string
	stderr   string
}

func runLoreWith(t *testing.T, plugins []lore.Plugin, args ...string) loreRun {
	t.Helper()

	stdout, stderr := captureFile(t, "stdout"), captureFile(t, "stderr")
	realArgs, realStdout, realStderr := os.Args, os.Stdout, os.Stderr
	os.Args, os.Stdout, os.Stderr = append([]string{"lore"}, args...), stdout, stderr

	var run loreRun
	returned := make(chan int, 1)
	go func() { returned <- app.Run(plugins...) }()

	select {
	case run.exitCode = <-returned:
		os.Args, os.Stdout, os.Stderr = realArgs, realStdout, realStderr
	case <-time.After(commandTimeout):
		t.Fatalf("`lore %s` has not returned after %s", strings.Join(args, " "), commandTimeout)
	}

	run.stdout, run.stderr = readCapture(t, stdout), readCapture(t, stderr)
	return run
}

func captureFile(t *testing.T, stream string) *os.File {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), stream+"-*.log")
	if err != nil {
		t.Fatalf("capture %s: %v", stream, err)
	}
	return f
}

func readCapture(t *testing.T, f *os.File) string {
	t.Helper()

	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", f.Name(), err)
	}
	written, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read %s: %v", f.Name(), err)
	}
	return string(written)
}

func writeLeakyConfig(t *testing.T, dir, body string) string {
	t.Helper()

	path := filepath.Join(dir, "lore.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func leakyWorkspace(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	return writeLeakyConfig(t, dir, "workspace: lore-e2e-redaction\n"+
		"index_path: "+strconv.Quote(filepath.Join(dir, "redaction.db"))+"\n"+
		leakySourceBlock+
		"embedder:\n"+
		"  provider: "+stubProviderName+"\n"+
		"  model: "+stubEmbedderModelName+"\n")
}

func leakyStatus(t *testing.T, pin string) loreRun {
	t.Helper()

	t.Setenv(leakyTokenEnv, leakyToken)
	t.Setenv(leakyPinEnv, pin)

	run := runLoreWith(t, []lore.Plugin{leakySourcePlugin{}, stubEmbedderPlugin{}}, "status", "--config", leakyWorkspace(t))
	if run.exitCode != exitOK {
		t.Fatalf("`lore status` exit = %d, stderr = %q", run.exitCode, run.stderr)
	}
	return run
}

func assertLoggedRedacted(t *testing.T, stderr string) {
	t.Helper()

	if !strings.Contains(stderr, leakyLoggedLine) {
		t.Fatalf("stderr = %q, want the plugin's log line", stderr)
	}
	if !strings.Contains(stderr, "token="+secrets.Placeholder) {
		t.Errorf("stderr = %q, want the logged token replaced by %s", stderr, secrets.Placeholder)
	}
	if strings.Contains(stderr, leakyToken) {
		t.Errorf("stderr = %q, want it free of the token", stderr)
	}
}

func TestACompiledInPluginsLoggedSecretReachesStderrRedacted(t *testing.T) {
	run := leakyStatus(t, leakyLongPin)

	assertLoggedRedacted(t, run.stderr)
	if !strings.Contains(run.stderr, "pin="+secrets.Placeholder) || strings.Contains(run.stderr, leakyLongPin) {
		t.Errorf("stderr = %q, want the logged pin replaced by %s", run.stderr, secrets.Placeholder)
	}
	if strings.Contains(run.stderr, "not scrubbed") {
		t.Errorf("stderr = %q, want no notice when every secret is long enough to scrub", run.stderr)
	}
}

func TestAShortSecretIsNoticedOnStderrAndLeavesStdoutAlone(t *testing.T) {
	scrubbed := leakyStatus(t, leakyLongPin)
	run := leakyStatus(t, leakyShort)

	assertLoggedRedacted(t, run.stderr)
	if got := strings.Count(run.stderr, leakyNotice); got != 1 {
		t.Errorf("stderr = %q, want the notice %q exactly once", run.stderr, leakyNotice)
	}
	if run.stdout != scrubbed.stdout {
		t.Errorf("stdout = %q, want it unchanged by the notice: %q", run.stdout, scrubbed.stdout)
	}
}

func TestPluginVerifyNoticesAShortSecretBeforeCertifying(t *testing.T) {
	t.Setenv(leakyTokenEnv, leakyToken)
	t.Setenv(leakyPinEnv, leakyShort)
	dir := t.TempDir()
	// A declaration names a local plugin by a ./ path; a Windows absolute path would not read as local.
	config := writeLeakyConfig(t, dir, "workspace: lore-e2e-redaction\n"+
		"plugins:\n"+
		"  - name: "+leakyPluginName+"\n"+
		"    from: ./"+buildExternalLeaky(t, dir)+"\n"+
		leakySourceBlock)

	run := runLoreWith(t, nil, "plugin", "verify", leakyPluginName, "--config", config)

	if got := strings.Count(run.stderr, leakyNotice); got != 1 {
		t.Errorf("exit = %d, stderr = %q, want the notice %q exactly once", run.exitCode, run.stderr, leakyNotice)
	}
	if strings.Contains(run.stdout, "not scrubbed") {
		t.Errorf("stdout = %q, want the notice on stderr only", run.stdout)
	}
}

func TestAnExternalPluginsStderrReachesTheHostLogRedacted(t *testing.T) {
	t.Setenv(leakyTokenEnv, leakyToken)
	t.Setenv(leakyPinEnv, leakyLongPin)
	dir := t.TempDir()
	binary := filepath.Join(dir, buildExternalLeaky(t, dir))

	var logged bytes.Buffer
	sink := &secrets.Sink{}
	host := lore.Host{Log: slog.New(slog.NewTextHandler(sink.Writer(&logged), &slog.HandlerOptions{Level: slog.LevelDebug}))}
	reg := registry.New(host, sink)
	plugin, err := plugexec.Open(context.Background(), binary, reg.Host(leakyPluginName))
	if err != nil {
		t.Fatalf("open the external plugin: %v", err)
	}
	if err := reg.RegisterExternal(registry.OriginExternal(binary), leakyPluginName, plugin); err != nil {
		t.Fatalf("register the external plugin: %v", err)
	}
	connectors, err := reg.BuildSources([]registry.Instance{{
		Use:   leakyPluginName,
		With:  map[string]any{"token_env": leakyTokenEnv, "pin_env": leakyPinEnv},
		Field: "sources[" + leakyPluginName + "]",
	}})
	if err != nil {
		t.Fatalf("build the external source: %v", err)
	}
	for _, err := range connectors[0].Changes(context.Background(), nil) {
		if err != nil {
			t.Fatalf("drain the external source: %v", err)
		}
	}

	if !strings.Contains(logged.String(), leakyStderrLine+secrets.Placeholder) {
		t.Errorf("host log = %q, want the plugin's stderr line with the token replaced by %s", logged.String(), secrets.Placeholder)
	}
	if strings.Contains(logged.String(), leakyToken) {
		t.Errorf("host log = %q, want it free of the token", logged.String())
	}
}

func buildExternalLeaky(t *testing.T, dir string) (name string) {
	t.Helper()

	source := filepath.Join(dir, "main.go")
	body := fmt.Sprintf(externalLeakySource, leakyPluginName, leakyStderrLine)
	if err := os.WriteFile(source, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", source, err)
	}

	name = "lore-" + leakyPluginName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", filepath.Join(dir, name), source).CombinedOutput(); err != nil {
		t.Fatalf("build the external plugin: %v\n%s", err, out)
	}
	return name
}
