package plugexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
)

// scriptedBinary is built once for the package: every test gets its own copy of
// it next to its own script, because a plugin gets no argv and no environment
// and so can be told what to do only by a file beside itself.
var scriptedBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "plugexec-fixture")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create the fixture directory:", err)
		os.Exit(1)
	}

	scriptedBinary = filepath.Join(dir, "scripted"+exeSuffix)
	build := exec.Command("go", "build", "-o", scriptedBinary, "./testdata/scripted")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "cannot build the scripted plugin: %v\n%s", buildErr, out)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

var exeSuffix = func() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}()

func newReader(text string) *bufio.Reader {
	return bufio.NewReaderSize(strings.NewReader(text), 64<<10)
}

// marshalRequest is the wire form of one request, for the tests that assert
// what does and does not cross the pipe.
func marshalRequest(req any) (string, error) {
	line, err := json.Marshal(req)
	return string(line), err
}

// The manifest lines every script starts with, one per kind. They are separate
// constants rather than a builder because a test that changes a manifest wants
// to read the manifest it is changing.
const (
	sourceManifest = `manifest emit {"v":1,"id":"$ID","ok":true,"manifest":{"name":"scripted","kind":"source","api_version":1,` +
		`"summary":"scripted fixture","capabilities":{"embed":false,"complete":false,"repo_remotes":false},"fields":[],"secrets":[]}}`

	providerManifest = `manifest emit {"v":1,"id":"$ID","ok":true,"manifest":{"name":"scripted","kind":"provider","api_version":1,` +
		`"summary":"scripted fixture","capabilities":{"embed":true,"complete":true,"repo_remotes":false},"fields":[],"secrets":[]}}`

	codeManifest = `manifest emit {"v":1,"id":"$ID","ok":true,"manifest":{"name":"scripted","kind":"code","api_version":1,` +
		`"summary":"scripted fixture","capabilities":{"embed":false,"complete":false,"repo_remotes":false},"fields":[],"secrets":[]}}`

	shutdownOK = `shutdown emit {"v":1,"id":"$ID","ok":true}`
)

// script assembles a script file from groups, which read better in a test than
// one string with hand-counted blank lines.
func script(groups ...string) string {
	return strings.Join(groups, "\n\n") + "\n"
}

// scripted writes a script beside a copy of the fixture binary and returns the
// path to execute.
func scripted(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()

	binary := filepath.Join(dir, "scripted"+exeSuffix)
	if err := os.Link(scriptedBinary, binary); err != nil {
		copyFile(t, scriptedBinary, binary)
	}
	if err := os.WriteFile(filepath.Join(dir, "script.txt"), []byte(text), 0o600); err != nil {
		t.Fatalf("writing the script: %v", err)
	}
	return binary
}

// copyFile is the fallback for a temp directory on another filesystem, where a
// hard link is not available.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("reading the fixture binary: %v", err)
	}
	if err := os.WriteFile(to, body, 0o700); err != nil {
		t.Fatalf("copying the fixture binary: %v", err)
	}
}

// testTuning keeps the shape of the protocol's timeouts — a longer budget for
// completions, a shorter one for the handshake — while staying short enough
// that a hung plugin fails the test instead of the CI job.
func testTuning() tuning {
	return tuning{
		manifest: 10 * time.Second,
		unary:    10 * time.Second,
		complete: 10 * time.Second,
		idle:     10 * time.Second,
		shutdown: 5 * time.Second,
		grace:    time.Second,
	}
}

// syncBuffer collects log output written from the process's stderr pump, which
// runs on its own goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testHost(logs io.Writer) lore.Host {
	if logs == nil {
		logs = io.Discard
	}
	return lore.Host{
		Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: time.Now,
	}
}

func openScript(t *testing.T, text string) (lore.Plugin, error) {
	t.Helper()
	return open(scripted(t, text), testHost(nil), testTuning())
}

func mustOpenScript(t *testing.T, text string) lore.Plugin {
	t.Helper()
	plugin, err := openScript(t, text)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return plugin
}

// connectorOf opens a source plugin and builds one instance of it.
func connectorOf(t *testing.T, text string, cfg lore.SourceConfig) lore.Connector {
	t.Helper()
	plugin := mustOpenScript(t, text)
	source, ok := plugin.(lore.SourcePlugin)
	if !ok {
		t.Fatalf("plugin is %T, want a lore.SourcePlugin", plugin)
	}
	conn, err := source.NewSource(cfg)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return conn
}

func drain(conn lore.Connector, cursor lore.Cursor) ([]lore.Batch, error) {
	var batches []lore.Batch
	for batch, err := range conn.Changes(context.Background(), cursor) {
		if err != nil {
			return batches, err
		}
		batches = append(batches, batch)
	}
	return batches, nil
}

func batchLine(docs, cursor string) string {
	return fmt.Sprintf(`changes emit {"v":1,"id":"$ID","batch":{"docs":[%s],"cursor":%s}}`, docs, cursor)
}

// ticket is one wire document with every required field, so a test that is not
// about document shape does not have to spell one out.
func ticket(instance, external string) string {
	return fmt.Sprintf(`{"id":"%s:ticket:%s","source":"%s","type":"ticket","repo_ref":"",`+
		`"title":"t","body":"b","author":"a","url":"https://example.test/%s",`+
		`"created_at":"2026-08-30T14:02:11Z","updated_at":"2026-09-01T07:45:03+02:00","refs":[]}`,
		instance, external, instance, external)
}

const doneLine = `changes emit {"v":1,"id":"$ID","done":true}`

func TestOpenReturnsOnlyTheKindTheManifestDeclares(t *testing.T) {
	plugin := mustOpenScript(t, script(sourceManifest, shutdownOK))

	if _, ok := plugin.(lore.SourcePlugin); !ok {
		t.Errorf("a source manifest produced %T, which is not a lore.SourcePlugin", plugin)
	}
	// The registry's kind check is only meaningful if a source cannot be bound
	// to a provider role by accident of interface satisfaction.
	if _, ok := plugin.(lore.ProviderPlugin); ok {
		t.Error("a source manifest produced a value that also satisfies lore.ProviderPlugin")
	}
	if _, ok := plugin.(lore.CodePlugin); ok {
		t.Error("a source manifest produced a value that also satisfies lore.CodePlugin")
	}
	if got := plugin.Manifest().Name; got != "scripted" {
		t.Errorf("Manifest().Name = %q, want %q", got, "scripted")
	}
}

func TestAPIVersionMismatchNamesBothVersions(t *testing.T) {
	mismatch := `manifest emit {"v":1,"id":"$ID","ok":true,"manifest":{"name":"future","kind":"source","api_version":2,` +
		`"summary":"s","capabilities":{"embed":false,"complete":false,"repo_remotes":false},"fields":[],"secrets":[]}}`

	_, err := openScript(t, script(mismatch, shutdownOK))
	if err == nil {
		t.Fatal("opened a plugin speaking api_version 2")
	}
	want := fmt.Sprintf("plugin %q speaks api_version 2, host speaks %d", "future", lore.APIVersion)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}

func TestManifestIsRequiredBeforeAnyOperation(t *testing.T) {
	// The plugin answers the handshake with an error, so no operation may run.
	refuse := `manifest emit {"v":1,"id":"$ID","error":{"message":"cannot read my own manifest","retryable":false,"kind":"internal"}}`

	_, err := openScript(t, script(refuse, shutdownOK))
	var pluginErr *Error
	if !errors.As(err, &pluginErr) {
		t.Fatalf("open error = %v (%T), want a *plugexec.Error", err, err)
	}
	if pluginErr.Op != opManifest || pluginErr.Kind != KindInternal {
		t.Errorf("error = %+v, want op %q kind %q", pluginErr, opManifest, KindInternal)
	}
}

// A source declaring repo_remotes answers remote questions over its own op, so
// the capability works the same out of process as in. Refusing it to external
// plugins would have made the unmatched-clone warning a privilege of compiled
// code, which is exactly the asymmetry the plugin contract exists to prevent.
func TestRepoRemotesIsAnsweredOverItsOwnOp(t *testing.T) {
	claims := `manifest emit {"v":1,"id":"$ID","ok":true,"manifest":{"name":"scripted","kind":"source","api_version":1,` +
		`"summary":"s","capabilities":{"embed":false,"complete":false,"repo_remotes":true},"fields":[],"secrets":[]}}`

	tests := []struct {
		name    string
		answer  string
		matches bool
	}{
		{name: "the instance ingests it", answer: `{"v":1,"id":"$ID","ok":true,"matches":true}`, matches: true},
		{name: "the instance does not", answer: `{"v":1,"id":"$ID","ok":true,"matches":false}`, matches: false},
		{
			// A startup warning must never be the reason a workspace fails to
			// start, so a plugin that cannot answer reads as "not mine" and the
			// operator gets the warning instead of an error.
			name:    "the plugin cannot answer",
			answer:  `{"v":1,"id":"$ID","error":{"message":"no idea","retryable":false,"kind":"internal"}}`,
			matches: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := connectorOf(t,
				script(claims, "matches_remote emit "+test.answer, shutdownOK),
				lore.SourceConfig{Instance: "scripted"})

			matcher, ok := conn.(lore.RemoteMatcher)
			if !ok {
				t.Fatalf("connector %T does not answer remote questions", conn)
			}
			if got := matcher.MatchesRemote("scripted:acme/app"); got != test.matches {
				t.Errorf("MatchesRemote = %v, want %v", got, test.matches)
			}
		})
	}
}

// An empty remote is never anybody's: a clone with no remote: entry is not a
// question worth spawning a process for.
func TestAnEmptyRemoteNeverReachesThePlugin(t *testing.T) {
	claims := `manifest emit {"v":1,"id":"$ID","ok":true,"manifest":{"name":"scripted","kind":"source","api_version":1,` +
		`"summary":"s","capabilities":{"embed":false,"complete":false,"repo_remotes":true},"fields":[],"secrets":[]}}`

	conn := connectorOf(t, script(claims, shutdownOK), lore.SourceConfig{Instance: "scripted"})
	if conn.(lore.RemoteMatcher).MatchesRemote("") {
		t.Error("MatchesRemote(\"\") = true, want false")
	}
}

func TestChangesStreamsBatchesThenDone(t *testing.T) {
	text := script(
		sourceManifest,
		batchLine(ticket("linear", "1")+","+ticket("linear", "2"), `{"after":"2"}`)+"\n"+
			batchLine("", `{"after":"2","idle":"true"}`)+"\n"+doneLine,
		shutdownOK,
	)

	batches, err := drain(connectorOf(t, text, lore.SourceConfig{Instance: "linear"}), nil)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2: %+v", len(batches), batches)
	}
	if got := len(batches[0].Docs); got != 2 {
		t.Errorf("first batch holds %d documents, want 2", got)
	}
	if got := batches[0].Cursor["after"]; got != "2" {
		t.Errorf("first cursor = %v, want after=2", batches[0].Cursor)
	}
	// An empty batch is still a checkpoint, so it reaches the consumer.
	if len(batches[1].Docs) != 0 || len(batches[1].Cursor) != 2 {
		t.Errorf("second batch = %+v, want no documents and a two-key cursor", batches[1])
	}
	if got := batches[0].Docs[0].UpdatedAt.Format(time.RFC3339); got != "2026-09-01T07:45:03+02:00" {
		t.Errorf("UpdatedAt = %s, want the offset the plugin sent", got)
	}
}

func TestBatchWithoutACursorIsRefused(t *testing.T) {
	for name, batch := range map[string]string{
		"absent": fmt.Sprintf(`changes emit {"v":1,"id":"$ID","batch":{"docs":[%s]}}`, ticket("linear", "1")),
		"empty":  batchLine(ticket("linear", "1"), `{}`),
		"null":   batchLine(ticket("linear", "1"), `null`),
	} {
		t.Run(name, func(t *testing.T) {
			text := script(sourceManifest, batch+"\n"+doneLine, shutdownOK)

			batches, err := drain(connectorOf(t, text, lore.SourceConfig{Instance: "linear"}), nil)
			if err == nil {
				t.Fatalf("a cursor-less batch was accepted: %+v", batches)
			}
			if len(batches) != 0 {
				t.Errorf("yielded %d batches before failing, want none", len(batches))
			}
			if !strings.Contains(err.Error(), "without a cursor") {
				t.Errorf("error %q does not say the batch carried no cursor", err)
			}
		})
	}
}

func TestStrayNonProtocolLineFailsTheOperation(t *testing.T) {
	text := script(
		sourceManifest,
		"changes raw Listening on stdin…\n"+batchLine(ticket("linear", "1"), `{"after":"1"}`)+"\n"+doneLine,
		shutdownOK,
	)

	batches, err := drain(connectorOf(t, text, lore.SourceConfig{Instance: "linear"}), nil)
	if err == nil {
		t.Fatalf("a banner on stdout was tolerated: %+v", batches)
	}
	// No resynchronization: the batch that followed the banner is never seen.
	if len(batches) != 0 {
		t.Errorf("yielded %d batches after the stray line, want none", len(batches))
	}
	if !strings.Contains(err.Error(), "not a protocol frame") {
		t.Errorf("error %q does not name the stray line", err)
	}
}

func TestLineOverTheLimitFailsTheOperationNamingInstanceAndOp(t *testing.T) {
	text := script(providerManifest, fmt.Sprintf("complete bigline %d", maxLineBytes+1), shutdownOK)

	plugin := mustOpenScript(t, text)
	provider, err := plugin.(lore.ProviderPlugin).NewProvider(lore.ProviderConfig{
		Instance:   "big",
		Capability: lore.CapabilityComplete,
		Model:      "m",
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.(lore.Completer).Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("a line over the limit was accepted")
	}
	for _, want := range []string{`"big"`, opComplete, "8 MiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestFrameWithTheWrongIDIsRefused(t *testing.T) {
	// The id is echoed verbatim on every frame, streamed frames included, so a
	// stream that starts correlated and then drifts is caught mid-stream.
	for name, stream := range map[string]string{
		"the first frame": `changes emit {"v":1,"id":"borrowed-from-the-docs","done":true}`,
		"a later batch": batchLine(ticket("linear", "1"), `{"after":"1"}`) + "\n" +
			`changes emit {"v":1,"id":"borrowed-from-the-docs","batch":{"docs":[],"cursor":{"after":"2"}}}` + "\n" + doneLine,
	} {
		t.Run(name, func(t *testing.T) {
			text := script(sourceManifest, stream, shutdownOK)

			_, err := drain(connectorOf(t, text, lore.SourceConfig{Instance: "linear"}), nil)
			if err == nil {
				t.Fatal("a frame echoing an id the host never sent was accepted")
			}
			if !strings.Contains(err.Error(), "borrowed-from-the-docs") {
				t.Errorf("error %q does not name the id the plugin sent", err)
			}
		})
	}
}

// The protocol's timeout table, which is the contract a plugin author writes
// against: shrinking one silently would fail slow backfills nobody changed.
func TestTheDefaultTimeoutsAreTheProtocolsOwn(t *testing.T) {
	tune := defaultTuning()
	for _, tt := range []struct {
		what string
		got  time.Duration
		want time.Duration
	}{
		{"manifest", tune.manifest, 10 * time.Second},
		{"embed, blame, log and has_file", tune.unary, 60 * time.Second},
		{"complete", tune.complete, lore.CompleteTimeout},
		{"changes while idle", tune.idle, 300 * time.Second},
		{"shutdown", tune.shutdown, 5 * time.Second},
		{"the grace before SIGKILL", tune.grace, 5 * time.Second},
	} {
		if tt.got != tt.want {
			t.Errorf("%s timeout = %v, want %v", tt.what, tt.got, tt.want)
		}
	}
}

func TestErrorKindsDecideWhetherSchedulingRetries(t *testing.T) {
	tests := []struct {
		name          string
		frame         string
		wantKind      ErrorKind
		wantRetryable bool
	}{
		{
			name:     "auth is never retried because credentials do not fix themselves",
			frame:    `{"message":"token lacks read:issues","retryable":false,"kind":"auth"}`,
			wantKind: KindAuth,
		},
		{
			name:     "invalid_config is never retried",
			frame:    `{"message":"teams is empty","retryable":false,"kind":"invalid_config"}`,
			wantKind: KindInvalidConfig,
		},
		{
			name:          "rate_limit implies retryable even unstated",
			frame:         `{"message":"slow down","retryable":false,"kind":"rate_limit"}`,
			wantKind:      KindRateLimit,
			wantRetryable: true,
		},
		{
			name:     "not_found fails the instance",
			frame:    `{"message":"no such team","retryable":false,"kind":"not_found"}`,
			wantKind: KindNotFound,
		},
		{
			name:     "an unknown kind is treated as internal",
			frame:    `{"message":"tea leaves unreadable","retryable":false,"kind":"astrological"}`,
			wantKind: KindInternal,
		},
		{
			name:          "retryable is authoritative whatever the kind",
			frame:         `{"message":"transient DNS failure","retryable":true,"kind":"internal"}`,
			wantKind:      KindInternal,
			wantRetryable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := script(
				sourceManifest,
				`changes emit {"v":1,"id":"$ID","error":`+tt.frame+`}`,
				shutdownOK,
			)

			_, err := drain(connectorOf(t, text, lore.SourceConfig{Instance: "linear"}), nil)
			var pluginErr *Error
			if !errors.As(err, &pluginErr) {
				t.Fatalf("error = %v (%T), want a *plugexec.Error", err, err)
			}
			if pluginErr.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", pluginErr.Kind, tt.wantKind)
			}
			if got := Retryable(err); got != tt.wantRetryable {
				t.Errorf("Retryable = %v, want %v", got, tt.wantRetryable)
			}
			if pluginErr.Instance != "linear" || pluginErr.Op != opChanges {
				t.Errorf("error = %+v, want instance linear and op changes", pluginErr)
			}
		})
	}
}

func TestUnknownKindKeepsWhatThePluginClaimed(t *testing.T) {
	text := script(
		sourceManifest,
		`changes emit {"v":1,"id":"$ID","error":{"message":"tea leaves unreadable","kind":"astrological"}}`,
		shutdownOK,
	)

	_, err := drain(connectorOf(t, text, lore.SourceConfig{Instance: "linear"}), nil)
	if err == nil || !strings.Contains(err.Error(), `"astrological"`) {
		t.Fatalf("error = %v, want the unknown kind quoted in the message", err)
	}
}

func TestErrorFrameLeavesTheProcessUsable(t *testing.T) {
	// An expected failure is an error frame, after which the process is still
	// alive and answers shutdown — which is what keeps it out of the crash path.
	text := script(
		sourceManifest,
		`changes emit {"v":1,"id":"$ID","error":{"message":"slow down","retryable":true,"kind":"rate_limit"}}`,
		shutdownOK+"\nshutdown exit 0",
	)

	_, err := drain(connectorOf(t, text, lore.SourceConfig{Instance: "linear"}), nil)
	var crash *CrashError
	if errors.As(err, &crash) {
		t.Fatalf("an error frame was reported as a crash: %v", crash)
	}
	if !Retryable(err) {
		t.Errorf("error %v is not retryable, want the rate limit backed off", err)
	}
}

func TestNonZeroExitIsACrashNamingInstanceAndOp(t *testing.T) {
	text := script(
		sourceManifest,
		batchLine(ticket("linear", "1"), `{"after":"1"}`)+"\nchanges exit 3",
		shutdownOK,
	)

	batches, err := drain(connectorOf(t, text, lore.SourceConfig{Instance: "linear"}), nil)
	if len(batches) != 1 {
		t.Fatalf("got %d batches before the crash, want the one that was sent", len(batches))
	}

	var crash *CrashError
	if !errors.As(err, &crash) {
		t.Fatalf("error = %v (%T), want a *plugexec.CrashError", err, err)
	}
	if crash.Instance != "linear" || crash.Op != opChanges {
		t.Errorf("crash = %+v, want instance linear and op changes", crash)
	}
	if !strings.Contains(crash.Error(), "exit status 3") {
		t.Errorf("crash %q does not report the exit status", crash)
	}
	// A crash is not a business error, so nothing schedules a retry off it.
	if Retryable(err) {
		t.Error("a crash was reported as retryable")
	}
}

func TestSecretsTravelInThePayloadAndTheEnvironmentIsNotInherited(t *testing.T) {
	const probe = "LORE_PLUGEXEC_PROBE"
	t.Setenv(probe, "a value the child must not see")

	text := script(
		providerManifest,
		`complete emit {"v":1,"id":"$ID","ok":true,"text":"secret=[$SECRET{api_key}] env=[$ENV{`+probe+`}]"}`,
		shutdownOK,
	)

	plugin := mustOpenScript(t, text)
	provider, err := plugin.(lore.ProviderPlugin).NewProvider(lore.ProviderConfig{
		Instance:   "scripted-llm",
		Capability: lore.CapabilityComplete,
		Model:      "m",
		Secrets:    map[string]string{"api_key": "sk-payload"},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	got, err := provider.(lore.Completer).Complete(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := "secret=[sk-payload]"; !strings.Contains(got, want) {
		t.Errorf("answer %q does not contain %q: secrets travel in the request payload", got, want)
	}
	// The deny case: a variable set in the parent is invisible to the child, so
	// a plugin sees only what its manifest declared.
	if !strings.Contains(got, "env=[]") {
		t.Errorf("answer %q shows the child read %s from its environment", got, probe)
	}
}

func TestSecretsNeverReachArgvOrTheChildEnvironment(t *testing.T) {
	// The other half of the guarantee, checked on the host side: the process is
	// executed with no arguments and an environment that carries no secret.
	session, err := spawn(scripted(t, script(sourceManifest, shutdownOK)), "linear", testHost(nil), testTuning())
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer session.abort()

	if len(session.cmd.Args) != 1 {
		t.Errorf("argv = %q, want only the binary: argv is world-readable in ps", session.cmd.Args)
	}
	if len(session.cmd.Env) != len(minimalEnv()) {
		t.Errorf("env = %q, want the minimal environment %q", session.cmd.Env, minimalEnv())
	}
	for _, entry := range session.cmd.Env {
		if strings.HasPrefix(entry, "LORE_") {
			t.Errorf("environment carries %q", entry)
		}
	}
}

func TestStderrIsForwardedToTheHostLoggerWithTheInstance(t *testing.T) {
	logs := &syncBuffer{}
	text := script(
		sourceManifest,
		"changes stderr fetching page 1 of 3\n"+batchLine("", `{"after":"0"}`)+"\n"+doneLine,
		shutdownOK,
	)

	plugin, err := open(scripted(t, text), testHost(logs), testTuning())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn, err := plugin.(lore.SourcePlugin).NewSource(lore.SourceConfig{Instance: "linear"})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if _, err := drain(conn, nil); err != nil {
		t.Fatalf("Changes: %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, `msg="linear: fetching page 1 of 3"`) {
		t.Errorf("logs do not carry the plugin's stderr prefixed with the instance:\n%s", out)
	}
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("plugin stderr was not logged at debug level:\n%s", out)
	}
}

func TestAnIdleStreamTimesOut(t *testing.T) {
	tune := testTuning()
	tune.idle = 150 * time.Millisecond

	text := script(
		sourceManifest,
		"changes sleep 3000\n"+doneLine,
		shutdownOK,
	)

	plugin, err := open(scripted(t, text), testHost(nil), tune)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn, err := plugin.(lore.SourcePlugin).NewSource(lore.SourceConfig{Instance: "linear"})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	start := time.Now()
	if _, err := drain(conn, nil); err == nil || !strings.Contains(err.Error(), "did not answer changes") {
		t.Fatalf("error = %v, want an idle timeout naming the op", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the idle timeout took %v, so the process was not escalated away", elapsed)
	}
}

func TestTheHandshakeRunsOnEveryProcessBeforeTheOperation(t *testing.T) {
	logs := &syncBuffer{}
	text := script(
		sourceManifest+"\nmanifest stderr handshaking",
		"changes stderr streaming\n"+batchLine("", `{"after":"0"}`)+"\n"+doneLine,
		shutdownOK,
	)

	plugin, err := open(scripted(t, text), testHost(logs), testTuning())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn, err := plugin.(lore.SourcePlugin).NewSource(lore.SourceConfig{Instance: "linear"})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if _, err := drain(conn, nil); err != nil {
		t.Fatalf("Changes: %v", err)
	}

	out := logs.String()
	// Two handshakes, one per process: the one Open ran and the one the stream's
	// own process ran before it was asked for anything.
	if got := strings.Count(out, "handshaking"); got != 2 {
		t.Errorf("the plugin handshook %d times, want one per process:\n%s", got, out)
	}
	handshake, streaming := strings.LastIndex(out, "handshaking"), strings.Index(out, "streaming")
	if handshake < 0 || streaming < 0 || handshake > streaming {
		t.Errorf("changes ran before the handshake of its own process:\n%s", out)
	}
}

func TestUnknownResponseFieldsAreIgnored(t *testing.T) {
	// Evolution is additive, and ignoring is what makes that safe: a plugin
	// built against a later host must still work against this one.
	future := `manifest emit {"v":1,"id":"$ID","ok":true,"trailer":{"deadline":"soon"},"manifest":{"name":"scripted",` +
		`"kind":"source","api_version":1,"summary":"s","capabilities":{"embed":false,"complete":false,"repo_remotes":false},` +
		`"fields":[],"secrets":[],"telemetry":true}}`
	batch := fmt.Sprintf(`changes emit {"v":1,"id":"$ID","batch":{"docs":[%s],"cursor":{"after":"1"},"partial":false},"eta":3}`,
		ticket("linear", "1"))

	batches, err := drain(connectorOf(t, script(future, batch+"\n"+doneLine, shutdownOK),
		lore.SourceConfig{Instance: "linear"}), nil)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(batches) != 1 || len(batches[0].Docs) != 1 {
		t.Fatalf("got %+v, want the one batch the plugin sent", batches)
	}
}

func TestCancellingTheContextEndsTheProcess(t *testing.T) {
	text := script(
		sourceManifest,
		batchLine(ticket("linear", "1"), `{"after":"1"}`)+"\nchanges sleep 5000\n"+doneLine,
		shutdownOK,
	)

	conn := connectorOf(t, text, lore.SourceConfig{Instance: "linear"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var last error
	seen := 0
	for _, err := range conn.Changes(ctx, nil) {
		if err != nil {
			last = err
			break
		}
		seen++
		cancel()
	}

	if seen != 1 {
		t.Fatalf("saw %d batches, want the one sent before the cancel", seen)
	}
	if last == nil || !errors.Is(last, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", last)
	}
}

func TestAbandoningTheStreamKillsThePlugin(t *testing.T) {
	// A consumer that stops pulling is a cancellation: the plugin must not be
	// left streaming into a pipe nobody reads.
	text := script(
		sourceManifest,
		batchLine(ticket("linear", "1"), `{"after":"1"}`)+"\n"+
			batchLine(ticket("linear", "2"), `{"after":"2"}`)+"\n"+doneLine,
		shutdownOK,
	)

	conn := connectorOf(t, text, lore.SourceConfig{Instance: "linear"})
	for batch, err := range conn.Changes(context.Background(), nil) {
		if err != nil {
			t.Fatalf("Changes: %v", err)
		}
		if len(batch.Docs) != 1 {
			t.Fatalf("batch = %+v, want one document", batch)
		}
		break
	}
	// The iterator's cleanup ran the escalation; a leaked process would keep the
	// test binary's temp directory busy and show up as a t.TempDir cleanup
	// failure on Windows, which is why this is worth asserting at all.
}

func TestTheRequestCarriesTheCursorAndTheInstance(t *testing.T) {
	text := script(
		sourceManifest,
		batchLine("", `{"echo":"$CURSOR{after}"}`)+"\n"+doneLine,
		shutdownOK,
	)

	batches, err := drain(connectorOf(t, text, lore.SourceConfig{Instance: "linear"}), lore.Cursor{"after": "41"})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if got := batches[0].Cursor["echo"]; got != "41" {
		t.Errorf("the plugin echoed cursor %q, want 41", got)
	}
}

func TestRequestsAreOneLineAndCarryTheEnvelope(t *testing.T) {
	// The framing is asserted on the host's own output rather than through a
	// plugin, because a body with a newline in it is exactly what would break a
	// plugin reading one line at a time.
	session := &session{instance: "linear", idPrefix: "abc"}
	env := session.begin(opChanges)

	line, err := json.Marshal(changesRequest{
		envelope: env,
		Instance: "linear",
		Config:   json.RawMessage(`{"note":"two\nlines"}`),
		Secrets:  map[string]string{"api_key": "sk-\n-injected"},
		Cursor:   lore.Cursor{"after": "1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.ContainsRune(line, '\n') {
		t.Errorf("request holds a raw newline: %s", line)
	}

	var decoded map[string]any
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"v", "id", "op", "instance", "config", "secrets", "cursor"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("request %s has no %q key", line, key)
		}
	}
	if decoded["v"] != float64(lore.APIVersion) || decoded["op"] != opChanges {
		t.Errorf("envelope = v %v op %v, want %d and %q", decoded["v"], decoded["op"], lore.APIVersion, opChanges)
	}
	if got := decoded["id"]; got != "abc-1" {
		t.Errorf("id = %v, want the host-generated abc-1", got)
	}
}

func TestAbsentConfigTravelsAsAnObject(t *testing.T) {
	// A plugin decoding `null` into its config struct would have to handle a
	// case a compiled plugin never sees.
	if got := string(emptyObject(nil)); got != "{}" {
		t.Errorf("emptyObject(nil) = %s, want {}", got)
	}
	if got := string(emptyObject(json.RawMessage(`{"a":1}`))); got != `{"a":1}` {
		t.Errorf("emptyObject rewrote a present config: %s", got)
	}
}

func TestReadLineRefusesALineOverTheLimit(t *testing.T) {
	// Unit-level so the limit itself is tested without moving 8 MiB through a
	// pipe on every run.
	huge := strings.Repeat("x", maxLineBytes+16)
	s := &session{instance: "linear", stdout: newReader(huge)}

	if _, err := s.readLine(); !errors.Is(err, errLineTooLong) {
		t.Fatalf("readLine error = %v, want errLineTooLong", err)
	}
}

func TestReadLineReturnsWholeFramesAcrossTheBufferBoundary(t *testing.T) {
	long := strings.Repeat("y", 200<<10)
	s := &session{instance: "linear", stdout: newReader("first\n" + long + "\n")}

	first, err := s.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if string(first) != "first" {
		t.Errorf("first line = %q, want %q", first, "first")
	}

	second, err := s.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if string(second) != long {
		t.Errorf("second line is %d bytes, want %d", len(second), len(long))
	}

	if _, err := s.readLine(); !errors.Is(err, io.EOF) {
		t.Errorf("third readLine error = %v, want EOF", err)
	}
}
