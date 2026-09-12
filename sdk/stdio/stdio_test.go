package stdio_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/stdio"
	"github.com/setthasit/Lore/sdk/wire"
)

const (
	pluginName    = "in-memory-notes"
	instance      = "fixture"
	secret        = "hunter2-7f3a1c"
	answerTimeout = 5 * time.Second
)

type changesFunc func(ctx context.Context, cursor lore.Cursor) iter.Seq2[lore.Batch, error]

type plugin struct{ changes changesFunc }

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       pluginName,
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "an in-memory source",
	}
}

func (p plugin) NewSource(config lore.SourceConfig) (lore.Connector, error) {
	return connector{name: config.Instance, changes: p.changes}, nil
}

type loggingPlugin struct {
	plugin
	log func(*slog.Logger)
}

func (p loggingPlugin) NewSource(config lore.SourceConfig) (lore.Connector, error) {
	p.log(config.Host.Log)
	return p.plugin.NewSource(config)
}

type connector struct {
	name    string
	changes changesFunc
}

func (c connector) Name() string { return c.name }

func (c connector) Changes(ctx context.Context, cursor lore.Cursor) iter.Seq2[lore.Batch, error] {
	return c.changes(ctx, cursor)
}

// A fully buffered input reaches EOF before the connector yields, which cancels
// the very request under test, so the pipe stays open until a case closes it.
type session struct {
	t        *testing.T
	requests *io.PipeWriter
	answers  *bufio.Scanner
	errOut   *syncBuffer
	returned chan error
	reaped   bool
	err      error
	ids      int
}

func serve(t *testing.T, p lore.Plugin) *session {
	t.Helper()

	in, requests := io.Pipe()
	answers, out := io.Pipe()
	s := &session{
		t:        t,
		requests: requests,
		answers:  answerScanner(answers),
		errOut:   &syncBuffer{},
		returned: make(chan error, 1),
	}

	go func() {
		err := stdio.ServeStreams(p, in, out, s.errOut)
		_ = out.Close()
		s.returned <- err
	}()

	overdue := time.AfterFunc(answerTimeout, func() { _ = answers.Close() })
	t.Cleanup(func() {
		overdue.Stop()
		_ = answers.Close()
		_ = requests.Close()
		_ = s.reap()
	})
	return s
}

func serveLogging(t *testing.T, log func(*slog.Logger)) *session {
	t.Helper()

	return serve(t, loggingPlugin{plugin: plugin{changes: never}, log: log})
}

func answerScanner(answers io.Reader) *bufio.Scanner {
	lines := bufio.NewScanner(answers)
	lines.Buffer(nil, wire.MaxLineBytes)
	return lines
}

func (s *session) envelope(op string) wire.Envelope {
	s.ids++
	return wire.Envelope{V: lore.APIVersion, ID: fmt.Sprintf("r-%d", s.ids), Op: op}
}

func (s *session) send(request any) {
	s.t.Helper()

	line, err := json.Marshal(request)
	if err != nil {
		s.t.Fatalf("marshal request: %v", err)
	}
	if _, err := s.requests.Write(append(line, '\n')); err != nil {
		s.t.Fatalf("send %s: %v", line, err)
	}
}

func (s *session) ask(op string) string {
	s.t.Helper()

	env := s.envelope(op)
	s.send(env)
	return env.ID
}

func (s *session) askChanges() string {
	s.t.Helper()

	return s.askChangesWithSecrets(nil)
}

func (s *session) askChangesWithSecrets(secrets map[string]string) string {
	s.t.Helper()

	env := s.envelope(wire.OpChanges)
	s.send(wire.ChangesRequest{Envelope: env, Instance: instance, Secrets: secrets, Cursor: lore.Cursor{}})
	return env.ID
}

func (s *session) next(wantID string) wire.Frame {
	s.t.Helper()

	if !s.answers.Scan() {
		s.t.Fatalf("no answer for %q: %v", wantID, s.answers.Err())
	}
	var frame wire.Frame
	if err := json.Unmarshal(s.answers.Bytes(), &frame); err != nil {
		s.t.Fatalf("answer %s is not a frame: %v", s.answers.Bytes(), err)
	}
	if frame.ID != wantID {
		s.t.Fatalf("answer carries id %q, want %q", frame.ID, wantID)
	}
	if frame.V != lore.APIVersion {
		s.t.Errorf("answer speaks v %d, want %d", frame.V, lore.APIVersion)
	}
	return frame
}

func (s *session) eof() error {
	s.t.Helper()

	if s.answers.Scan() {
		s.t.Fatalf("a further answer followed: %s", s.answers.Bytes())
	}
	return s.reap()
}

func (s *session) reap() error {
	s.t.Helper()

	if s.reaped {
		return s.err
	}
	s.reaped = true
	select {
	case s.err = <-s.returned:
	case <-time.After(answerTimeout):
		s.t.Error("ServeStreams did not return once its streams closed")
	}
	return s.err
}

func (s *session) closeInput() {
	s.t.Helper()

	if err := s.requests.Close(); err != nil {
		s.t.Fatalf("close the request stream: %v", err)
	}
}

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

func never(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(func(lore.Batch, error) bool) {}
}

func batch(cursor string, docs ...lore.Document) lore.Batch {
	return lore.Batch{Docs: docs, Cursor: lore.Cursor{"after": cursor}}
}

func assertError(t *testing.T, frame wire.Frame, wantKind string) *wire.Error {
	t.Helper()

	if frame.Error == nil {
		t.Fatalf("frame carries no error: %+v", frame)
	}
	if frame.Done {
		t.Error("an error frame also reports done")
	}
	if frame.Error.Kind != wantKind {
		t.Errorf("kind = %q, want %q", frame.Error.Kind, wantKind)
	}
	return frame.Error
}

func TestManifestIsAnsweredWithTheRequestID(t *testing.T) {
	s := serve(t, plugin{changes: never})

	frame := s.next(s.ask(wire.OpManifest))
	if !frame.OK {
		t.Errorf("manifest frame reports ok=false: %+v", frame)
	}
	if frame.Manifest == nil {
		t.Fatal("manifest frame carries no manifest")
	}
	if frame.Manifest.Name != pluginName || frame.Manifest.Kind != lore.KindSource {
		t.Errorf("manifest = %+v, want the plugin's own", frame.Manifest)
	}
}

func TestChangesStreamsACursorPerBatchThenOneDone(t *testing.T) {
	cursors := make(chan lore.Cursor, 1)
	s := serve(t, plugin{changes: func(_ context.Context, cursor lore.Cursor) iter.Seq2[lore.Batch, error] {
		cursors <- cursor
		return func(yield func(lore.Batch, error) bool) {
			for _, b := range []lore.Batch{batch("1", lore.Document{ID: "fixture:note:1"}), batch("2")} {
				if !yield(b, nil) {
					return
				}
			}
		}
	}})

	changes := s.askChanges()
	for _, want := range []struct {
		docs   int
		cursor string
	}{{docs: 1, cursor: "1"}, {docs: 0, cursor: "2"}} {
		frame := s.next(changes)
		if frame.Batch == nil {
			t.Fatalf("frame carries no batch: %+v", frame)
		}
		if len(frame.Batch.Docs) != want.docs {
			t.Errorf("batch holds %d documents, want %d", len(frame.Batch.Docs), want.docs)
		}
		if frame.Batch.Cursor == nil || (*frame.Batch.Cursor)["after"] != want.cursor {
			t.Errorf("batch cursor = %v, want after=%s", frame.Batch.Cursor, want.cursor)
		}
		if frame.Done {
			t.Error("a batch frame also reports done")
		}
	}

	if frame := s.next(changes); !frame.Done || frame.Error != nil {
		t.Errorf("last frame = %+v, want done", frame)
	}
	if cursor := <-cursors; cursor != nil {
		t.Errorf("a first sync reached the connector with cursor %v, want nil", cursor)
	}

	if frame := s.next(s.ask(wire.OpShutdown)); !frame.OK {
		t.Errorf("shutdown frame = %+v, want ok", frame)
	}
}

func TestACursorlessBatchIsRefusedAndStopsTheStream(t *testing.T) {
	s := serve(t, plugin{changes: func(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
		return func(yield func(lore.Batch, error) bool) {
			if !yield(lore.Batch{Docs: []lore.Document{{ID: "fixture:note:1"}, {ID: "fixture:note:2"}}}, nil) {
				return
			}
			yield(batch("2"), nil)
		}
	}})

	changes := s.askChanges()
	failure := assertError(t, s.next(changes), wire.KindInternal)
	if !strings.Contains(failure.Message, instance) {
		t.Errorf("refusal %q does not name the connector whose batch it refused", failure.Message)
	}

	if frame := s.next(s.ask(wire.OpShutdown)); !frame.OK {
		t.Errorf("shutdown frame = %+v, want ok", frame)
	}
}

func TestAnUnknownOperationIsAnsweredAndTheLoopKeepsServing(t *testing.T) {
	s := serve(t, plugin{changes: never})

	unknown := s.ask("reticulate")
	failure := assertError(t, s.next(unknown), wire.KindInternal)
	if !strings.Contains(failure.Message, "reticulate") {
		t.Errorf("error %q does not name the operation", failure.Message)
	}

	if frame := s.next(s.ask(wire.OpManifest)); !frame.OK {
		t.Errorf("manifest frame = %+v, want ok", frame)
	}
}

func TestARequestWithoutAnIDIsReportedAndNotAnswered(t *testing.T) {
	s := serve(t, plugin{changes: never})

	s.send(wire.Envelope{V: lore.APIVersion, Op: wire.OpManifest})

	if frame := s.next(s.ask(wire.OpManifest)); !frame.OK {
		t.Errorf("manifest frame = %+v, want ok", frame)
	}
	if s.errOut.String() == "" {
		t.Error("the id-less request was dropped without a word on the error stream")
	}
}

func TestShutdownIsAnsweredBeforeTheLoopReturns(t *testing.T) {
	s := serve(t, plugin{changes: never})

	if frame := s.next(s.ask(wire.OpShutdown)); !frame.OK {
		t.Errorf("shutdown frame = %+v, want ok", frame)
	}
	if err := s.eof(); err != nil {
		t.Errorf("ServeStreams = %v, want nil after shutdown", err)
	}
}

func TestEndOfInputCancelsTheOperationInFlight(t *testing.T) {
	entered := make(chan struct{})
	s := serve(t, plugin{changes: func(ctx context.Context, _ lore.Cursor) iter.Seq2[lore.Batch, error] {
		return func(func(lore.Batch, error) bool) {
			close(entered)
			<-ctx.Done()
		}
	}})

	changes := s.askChanges()
	<-entered
	s.closeInput()

	failure := assertError(t, s.next(changes), wire.KindInternal)
	if failure.Message != context.Canceled.Error() {
		t.Errorf("failure = %q, want %q", failure.Message, context.Canceled.Error())
	}
	if err := s.eof(); err != nil {
		t.Errorf("ServeStreams = %v, want nil after end of input", err)
	}
}

func TestAPanicBeforeAnyFrameIsAnsweredWithoutThePanicValue(t *testing.T) {
	const reason = "the connector tore a hole in its own iterator"
	s := serve(t, plugin{changes: func(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
		return func(func(lore.Batch, error) bool) { panic(reason) }
	}})

	changes := s.askChanges()
	failure := assertError(t, s.next(changes), wire.KindInternal)
	if strings.Contains(failure.Message, reason) {
		t.Errorf("the answer carries the panic value: %q", failure.Message)
	}

	shutdown := s.ask(wire.OpShutdown)
	if frame := s.next(shutdown); !frame.OK {
		t.Errorf("shutdown frame = %+v, want ok", frame)
	}
	if !strings.Contains(s.errOut.String(), reason) {
		t.Errorf("the error stream does not report the panic:\n%s", s.errOut)
	}
}

func TestAPanicAfterTheTerminalFrameIsReportedWithoutASecondFrame(t *testing.T) {
	const reason = "the connector panicked once it was stopped"
	s := serve(t, plugin{changes: func(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
		return func(yield func(lore.Batch, error) bool) {
			yield(lore.Batch{}, errors.New("the source went away"))
			panic(reason)
		}
	}})

	changes := s.askChanges()
	assertError(t, s.next(changes), wire.KindInternal)

	if frame := s.next(s.ask(wire.OpShutdown)); !frame.OK {
		t.Errorf("shutdown frame = %+v, want ok", frame)
	}
	if !strings.Contains(s.errOut.String(), reason) {
		t.Errorf("the error stream does not report the panic:\n%s", s.errOut)
	}
}

var roundSecrets = map[string]string{"api_token": secret}

func TestALoggedStringLosesTheSecretAndAScalarPrintsAsItself(t *testing.T) {
	s := serveLogging(t, func(log *slog.Logger) {
		log.Info("opening the notes",
			slog.String("endpoint", "https://notes.example/sync?token="+secret),
			slog.Int("attempts", 3))
	})

	if frame := s.next(s.askChangesWithSecrets(roundSecrets)); !frame.Done {
		t.Errorf("changes frame = %+v, want done", frame)
	}

	diagnostics := s.errOut.String()
	if strings.Contains(diagnostics, secret) {
		t.Errorf("the error stream carries the round's secret:\n%s", diagnostics)
	}
	if !strings.Contains(diagnostics, "https://notes.example/sync?token=[redacted]") {
		t.Errorf("the attribute lost more than the secret:\n%s", diagnostics)
	}
	if !strings.Contains(diagnostics, "attempts=3") {
		t.Errorf("the scalar attribute does not print as itself:\n%s", diagnostics)
	}
}

func TestADeeplyNestedLoggedValueIsCappedRatherThanWalkedWithoutEnd(t *testing.T) {
	s := serveLogging(t, func(log *slog.Logger) {
		value := slog.StringValue("the innermost note")
		for range 150 {
			value = slog.GroupValue(slog.Attr{Key: "nested", Value: value})
		}
		log.Info("opening the notes", slog.Attr{Key: "trail", Value: value})
	})

	if frame := s.next(s.askChanges()); !frame.Done {
		t.Errorf("changes frame = %+v, want done", frame)
	}
	if diagnostics := s.errOut.String(); !strings.Contains(diagnostics, "[too deeply nested]") {
		t.Errorf("the walk never reports its depth cap:\n%s", diagnostics)
	}
}

func TestASecretInAPanicReasonIsRedactedOnTheErrorStream(t *testing.T) {
	for _, reason := range []struct {
		name  string
		value any
	}{
		{name: "a string reason", value: "the notes API refused " + secret},
		{name: "an error reason", value: fmt.Errorf("the notes API refused %s", secret)},
	} {
		t.Run(reason.name, func(t *testing.T) {
			s := serve(t, plugin{changes: func(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
				return func(func(lore.Batch, error) bool) { panic(reason.value) }
			}})

			assertError(t, s.next(s.askChangesWithSecrets(roundSecrets)), wire.KindInternal)
			if frame := s.next(s.ask(wire.OpShutdown)); !frame.OK {
				t.Errorf("shutdown frame = %+v, want ok", frame)
			}

			diagnostics := s.errOut.String()
			if strings.Contains(diagnostics, secret) {
				t.Errorf("the error stream carries the round's secret:\n%s", diagnostics)
			}
			if !strings.Contains(diagnostics, "the notes API refused [redacted]") {
				t.Errorf("the panic reason lost more than the secret:\n%s", diagnostics)
			}
		})
	}
}

type credentials struct{ Token string }

func TestALoggedCompositeValueIsNamedByItsTypeAndNotRendered(t *testing.T) {
	s := serveLogging(t, func(log *slog.Logger) {
		log.Info("opening the notes", slog.Any("credentials", credentials{Token: secret}))
	})

	if frame := s.next(s.askChangesWithSecrets(roundSecrets)); !frame.Done {
		t.Errorf("changes frame = %+v, want done", frame)
	}

	diagnostics := s.errOut.String()
	if strings.Contains(diagnostics, secret) {
		t.Errorf("the composite value was rendered, secret and all:\n%s", diagnostics)
	}
	if want := fmt.Sprintf("[value of type %T omitted]", credentials{}); !strings.Contains(diagnostics, want) {
		t.Errorf("the error stream does not name the value's type as %q:\n%s", want, diagnostics)
	}
}

type unprintableError struct{}

func (unprintableError) Error() string { panic("the author's Error method is broken") }

func TestALoggedErrorThatPanicsCostsNeitherTheRoundNorTheRecord(t *testing.T) {
	s := serveLogging(t, func(log *slog.Logger) {
		log.Info("opening the notes", slog.Any("cause", unprintableError{}))
	})

	frame := s.next(s.askChanges())
	if !frame.Done || frame.Error != nil {
		t.Errorf("changes frame = %+v, want done", frame)
	}
	if diagnostics := s.errOut.String(); !strings.Contains(diagnostics, "opening the notes") {
		t.Errorf("the record never reached the error stream:\n%s", diagnostics)
	}
}

func TestAnAuthorsFailureKindReachesTheWire(t *testing.T) {
	for _, reported := range []struct {
		name  string
		cause error
		kind  string
	}{
		{
			name:  "an unkinded failure defers to its cause",
			cause: lore.Failure{Err: lore.Failure{Kind: wire.KindAuth, Err: errors.New("the token expired")}},
			kind:  wire.KindAuth,
		},
		{
			name:  "a failure yielded by pointer",
			cause: &lore.Failure{Kind: wire.KindRateLimit, Err: errors.New("too many reads this minute")},
			kind:  wire.KindRateLimit,
		},
		{
			name:  "a failure wrapped by a plain error",
			cause: fmt.Errorf("opening %q: %w", instance, lore.Failure{Kind: wire.KindNotFound, Err: errors.New("no such notebook")}),
			kind:  wire.KindNotFound,
		},
		{
			name:  "a kind the protocol does not publish",
			cause: lore.Failure{Kind: "astrological", Err: errors.New("mercury is in retrograde")},
			kind:  "astrological",
		},
	} {
		t.Run(reported.name, func(t *testing.T) {
			s := serve(t, plugin{changes: func(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
				return func(yield func(lore.Batch, error) bool) { yield(lore.Batch{}, reported.cause) }
			}})

			assertError(t, s.next(s.askChanges()), reported.kind)
		})
	}
}
