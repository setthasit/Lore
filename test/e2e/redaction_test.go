package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	leakyEchoedID   = "token="
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
	return source{name: c.Instance, token: c.Secret("token")}, nil
}

type source struct{ name, token string }

func (s source) Name() string { return s.name }

func (s source) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) {
		yield(lore.Batch{
			Docs:   []lore.Document{{ID: lore.NewDocID(s.name, lore.DocTypePage, %q+s.token)}},
			Cursor: lore.Cursor{"page": "1"},
		}, nil)
	}
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

func leakyWorkspace(t *testing.T, sources string) string {
	t.Helper()

	dir := t.TempDir()
	return writeLeakyConfig(t, dir, "workspace: lore-e2e-redaction\n"+
		"index_path: "+strconv.Quote(filepath.Join(dir, "redaction.db"))+"\n"+
		sources+
		"embedder:\n"+
		"  provider: "+stubProviderName+"\n"+
		"  model: "+stubEmbedderModelName+"\n")
}

func leakyStatus(t *testing.T, pin string) loreRun {
	t.Helper()

	t.Setenv(leakyTokenEnv, leakyToken)
	t.Setenv(leakyPinEnv, pin)

	run := runLoreWith(t, []lore.Plugin{leakySourcePlugin{}, stubEmbedderPlugin{}}, "status", "--config", leakyWorkspace(t, leakySourceBlock))
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

const (
	refusingPluginName = "e2e-refusing"
	refusingTokenEnv   = "LORE_E2E_REFUSING_TOKEN"
	fakeTokenStem      = "FAKE-"
	sourceRefusal      = "source refused token "

	unsetTokenEnv = "LORE_E2E_UNSET_TOKEN"
)

type refusingSourcePlugin struct{}

var _ lore.SourcePlugin = refusingSourcePlugin{}

func (refusingSourcePlugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       refusingPluginName,
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "compiled-in source whose upstream quotes its token in every refusal",
		Secrets:    []lore.Secret{{Key: "token", ConfigField: "token_env"}},
	}
}

func (refusingSourcePlugin) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	return refusingConnector{name: c.Instance, token: c.Secret("token")}, nil
}

type refusingConnector struct{ name, token string }

func (c refusingConnector) Name() string { return c.name }

func (c refusingConnector) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) { yield(lore.Batch{}, errors.New(sourceRefusal+c.token)) }
}

func TestASyncFailureQuotingASecretReachesStderrRedacted(t *testing.T) {
	for name, token := range map[string]string{
		"plain":                  fakeTokenStem + "src-tok-9Qx7Lm2Zp",
		"control byte and quote": fakeTokenStem + "tab\there\"quoted-99",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(refusingTokenEnv, token)
			config := leakyWorkspace(t, "sources:\n"+
				"  - use: "+refusingPluginName+"\n"+
				"    with:\n"+
				"      token_env: "+refusingTokenEnv+"\n")

			run := runLoreWith(t, []lore.Plugin{refusingSourcePlugin{}, stubEmbedderPlugin{}}, "sync", "--config", config)

			if run.exitCode == exitOK {
				t.Fatalf("`lore sync` succeeded over a refusing source; stdout = %q", run.stdout)
			}
			if !strings.Contains(run.stderr, sourceRefusal+secrets.Placeholder) {
				t.Errorf("stderr = %q, want the source's refusal with the token replaced by %s", run.stderr, secrets.Placeholder)
			}
			if strings.Contains(run.stdout+run.stderr, fakeTokenStem) {
				t.Errorf("stdout = %q, stderr = %q, want both free of the token in any form", run.stdout, run.stderr)
			}
		})
	}
}

func TestAFailedStartupNoticesAShortSecretBeforeItsError(t *testing.T) {
	t.Setenv(leakyTokenEnv, leakyToken)
	t.Setenv(leakyPinEnv, leakyShort)
	t.Setenv(unsetTokenEnv, "")
	config := leakyWorkspace(t, leakySourceBlock+
		"  - id: unset\n"+
		"    use: "+leakyPluginName+"\n"+
		"    with:\n"+
		"      token_env: "+unsetTokenEnv+"\n"+
		"      pin_env: "+leakyPinEnv+"\n")

	run := runLoreWith(t, []lore.Plugin{leakySourcePlugin{}, stubEmbedderPlugin{}}, "status", "--config", config)

	if run.exitCode == exitOK {
		t.Fatalf("`lore status` started without %s; stdout = %q", unsetTokenEnv, run.stdout)
	}
	if got := strings.Count(run.stderr, leakyNotice); got != 1 {
		t.Fatalf("stderr = %q, want the notice %q exactly once", run.stderr, leakyNotice)
	}
	failure := strings.Index(run.stderr, "lore: sources[unset]")
	if failure < 0 || strings.Index(run.stderr, leakyNotice) > failure {
		t.Errorf("stderr = %q, want the notice before the startup error", run.stderr)
	}
}

func TestAnInstanceIDCarryingControlBytesReachesStderrEscaped(t *testing.T) {
	t.Setenv(leakyTokenEnv, leakyToken)
	t.Setenv(leakyPinEnv, leakyLongPin)
	config := leakyWorkspace(t, "sources:\n"+
		"  - id: \"\\e[31mBOOM\\a\"\n"+
		"    use: "+leakyPluginName+"\n")

	run := runLoreWith(t, []lore.Plugin{leakySourcePlugin{}, stubEmbedderPlugin{}}, "status", "--config", config)

	if run.exitCode == exitOK {
		t.Fatalf("`lore status` accepted a control-byte id; stdout = %q", run.stdout)
	}
	if !strings.Contains(run.stderr, `lore: sources[\x1b[31mBOOM\a]`) {
		t.Errorf("stderr = %q, want the id's control bytes shown escaped", run.stderr)
	}
	if strings.ContainsAny(run.stderr, "\x1b\a") {
		t.Errorf("stderr = %q, want it free of raw control bytes", run.stderr)
	}
}

func TestPluginVerifyNoticesAShortSecretBeforeCertifying(t *testing.T) {
	t.Setenv(leakyTokenEnv, leakyToken)
	t.Setenv(leakyPinEnv, leakyShort)

	run := runLoreWith(t, nil, "plugin", "verify", leakyPluginName, "--config", verifyWorkspace(t))

	if got := strings.Count(run.stderr, leakyNotice); got != 1 {
		t.Errorf("exit = %d, stderr = %q, want the notice %q exactly once", run.exitCode, run.stderr, leakyNotice)
	}
	if strings.Contains(run.stdout, "not scrubbed") {
		t.Errorf("stdout = %q, want the notice on stderr only", run.stdout)
	}
}

func TestPluginVerifyPrintsAFailureQuotingASecretRedacted(t *testing.T) {
	t.Setenv(leakyTokenEnv, leakyToken)
	t.Setenv(leakyPinEnv, leakyLongPin)

	run := runLoreWith(t, nil, "plugin", "verify", leakyPluginName, "--config", verifyWorkspace(t))

	if !strings.Contains(run.stdout, leakyEchoedID+secrets.Placeholder) {
		t.Errorf("exit = %d, stdout = %q, want the failed check's document id with the token replaced by %s",
			run.exitCode, run.stdout, secrets.Placeholder)
	}
	if strings.Contains(run.stdout+run.stderr, leakyToken) {
		t.Errorf("stdout = %q, stderr = %q, want both free of the token", run.stdout, run.stderr)
	}
}

func verifyWorkspace(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	// A declaration names a local plugin by a ./ path; a Windows absolute path would not read as local.
	return writeLeakyConfig(t, dir, "workspace: lore-e2e-redaction\n"+
		"plugins:\n"+
		"  - name: "+leakyPluginName+"\n"+
		"    from: ./"+buildExternalLeaky(t, dir)+"\n"+
		leakySourceBlock)
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
	body := fmt.Sprintf(externalLeakySource, leakyPluginName, leakyStderrLine, leakyEchoedID)
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

const (
	echoingProviderName = "e2e-echoing"
	echoingTokenEnv     = "LORE_E2E_ECHOING_TOKEN"
	echoingToken        = "fake-echoing-token-9c4e"
)

type echoingEmbedderPlugin struct{}

var _ lore.ProviderPlugin = echoingEmbedderPlugin{}

func (echoingEmbedderPlugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:          echoingProviderName,
		Kind:          lore.KindProvider,
		APIVersion:    lore.APIVersion,
		Summary:       "embedder whose upstream echoes its token in every refusal",
		Capabilities:  lore.Capabilities{Embed: true},
		DefaultModels: map[lore.Capability]string{lore.CapabilityEmbed: stubEmbedderModelName},
		Secrets:       []lore.Secret{{Key: "token", ConfigField: "token_env"}},
	}
}

func (echoingEmbedderPlugin) NewProvider(c lore.ProviderConfig) (lore.Provider, error) {
	return echoingEmbedder{token: c.Secret("token")}, nil
}

type echoingEmbedder struct{ token string }

func (e echoingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("upstream rejected token " + e.token)
}

func (echoingEmbedder) Dimensions() int { return 8 }

func TestAnMCPToolFailureCarryingASecretReachesStderrRedacted(t *testing.T) {
	t.Setenv(leakyTokenEnv, leakyToken)
	t.Setenv(leakyPinEnv, leakyLongPin)
	t.Setenv(echoingTokenEnv, echoingToken)
	dir := t.TempDir()
	config := writeLeakyConfig(t, dir, "workspace: lore-e2e-redaction\n"+
		"index_path: "+strconv.Quote(filepath.Join(dir, "redaction.db"))+"\n"+
		leakySourceBlock+
		"providers:\n"+
		"  - use: "+echoingProviderName+"\n"+
		"    with:\n"+
		"      token_env: "+echoingTokenEnv+"\n"+
		"embedder:\n"+
		"  provider: "+echoingProviderName+"\n"+
		"  model: "+stubEmbedderModelName+"\n")

	_, logged := callMCPTool(t, []lore.Plugin{leakySourcePlugin{}, echoingEmbedderPlugin{}},
		config, findDecisionTool, `{"question":"why sqlite?"}`)

	failure := toolFailureLine(logged, findDecisionTool)
	if failure == "" {
		t.Fatalf("stderr = %q, want the %s failure logged", logged, findDecisionTool)
	}
	if !strings.Contains(failure, "token "+secrets.Placeholder) {
		t.Errorf("failure line = %q, want the echoed token replaced by %s", failure, secrets.Placeholder)
	}
	if strings.Contains(logged, echoingToken) {
		t.Errorf("stderr = %q, want it free of the echoed token", logged)
	}
}

const (
	misfilingPluginName = "e2e-misfiling"
	misfilingTokenEnv   = "LORE_E2E_MISFILING_TOKEN"
	misfilingToken      = "fake-misfiling-token-4b8d"
	misfiledIDPrefix    = "elsewhere:page:token="
)

type misfilingSourcePlugin struct{}

var _ lore.SourcePlugin = misfilingSourcePlugin{}

func (misfilingSourcePlugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       misfilingPluginName,
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "compiled-in source that files a document holding its token under another namespace",
		Secrets:    []lore.Secret{{Key: "token", ConfigField: "token_env"}},
	}
}

func (misfilingSourcePlugin) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	return misfilingConnector{name: c.Instance, token: c.Secret("token")}, nil
}

type misfilingConnector struct{ name, token string }

func (c misfilingConnector) Name() string { return c.name }

func (c misfilingConnector) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) {
		yield(lore.Batch{
			Docs:   []lore.Document{{ID: lore.DocID(misfiledIDPrefix + c.token), Source: c.name}},
			Cursor: lore.Cursor{"page": "1"},
		}, nil)
	}
}

func TestAnMCPSyncFailureNamingASecretReachesStdoutRedacted(t *testing.T) {
	t.Setenv(misfilingTokenEnv, misfilingToken)
	config := leakyWorkspace(t, "sources:\n"+
		"  - use: "+misfilingPluginName+"\n"+
		"    with:\n"+
		"      token_env: "+misfilingTokenEnv+"\n")

	response, _ := callMCPTool(t, []lore.Plugin{misfilingSourcePlugin{}, stubEmbedderPlugin{}}, config, syncNowTool, `{}`)

	if !strings.Contains(response, misfiledIDPrefix+secrets.Placeholder) {
		t.Errorf("sync_now response = %q, want the misfiled document id with the token replaced by %s",
			response, secrets.Placeholder)
	}
	if strings.Contains(response, misfilingToken) {
		t.Errorf("sync_now response = %q, want it free of the token", response)
	}
}

func callMCPTool(t *testing.T, plugins []lore.Plugin, config, tool, arguments string) (response, logged string) {
	t.Helper()

	stdin, requests, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdin: %v", err)
	}
	responses, stdout, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	if err := responses.SetReadDeadline(time.Now().Add(commandTimeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	stderr := captureFile(t, "stderr")
	realArgs, realStdin, realStdout, realStderr := os.Args, os.Stdin, os.Stdout, os.Stderr
	os.Args = []string{"lore", "mcp", "--config", config}
	os.Stdin, os.Stdout, os.Stderr = stdin, stdout, stderr
	t.Cleanup(func() {
		os.Args, os.Stdin, os.Stdout, os.Stderr = realArgs, realStdin, realStdout, realStderr
		for _, f := range []*os.File{stdin, requests, responses, stdout} {
			_ = f.Close()
		}
	})

	returned := make(chan int, 1)
	go func() { returned <- app.Run(plugins...) }()

	for _, message := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"lore-e2e","version":"v0.0.1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + arguments + `}}`,
	} {
		if _, err := requests.WriteString(message + "\n"); err != nil {
			t.Fatalf("write %s: %v", message, err)
		}
	}
	response = awaitResponse(t, bufio.NewScanner(responses), 2, stderr.Name())
	_ = requests.Close()

	select {
	case <-returned:
		os.Stdout, os.Stderr = realStdout, realStderr
	case <-time.After(commandTimeout):
		t.Fatalf("`lore mcp` has not returned %s after its client hung up", commandTimeout)
	}
	return response, readCapture(t, stderr)
}

func toolFailureLine(logged, tool string) string {
	for line := range strings.Lines(logged) {
		if strings.Contains(line, `msg="`+tool+` failed"`) {
			return line
		}
	}
	return ""
}

func awaitResponse(t *testing.T, stdout *bufio.Scanner, id int, stderrPath string) string {
	t.Helper()

	for stdout.Scan() {
		var response struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatalf("stdout line %q is not JSON-RPC: %v", stdout.Text(), err)
		}
		if response.ID == id {
			return stdout.Text()
		}
	}
	logged, _ := os.ReadFile(stderrPath)
	t.Fatalf("no response with id %d on stdout: %v; stderr = %q", id, stdout.Err(), logged)
	return ""
}
