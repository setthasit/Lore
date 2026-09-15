// Package stdio runs a plugin's side of the NDJSON protocol: NDJSON requests
// in, one answer per line out, diagnostics on a separate error stream. A
// request line over the protocol's cap ends the loop, because no id can be
// recovered from it. A connector's string or error diagnostics lose the
// round's secrets; a scalar prints as itself and any other value is named.
package stdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/wire"
)

// Serve answers the host's requests on the process's own streams until stdin
// reaches EOF or the host asks for shutdown.
func Serve(plugin lore.Plugin) error {
	return ServeStreams(plugin, os.Stdin, os.Stdout, os.Stderr)
}

// ServeStreams is Serve over streams the caller chooses; out carries answers and
// errOut diagnostics and logs. Its read of in outlives the return until in closes.
func ServeStreams(plugin lore.Plugin, in io.Reader, out, errOut io.Writer) error {
	s := server{plugin: plugin, out: out, errOut: &lockedWriter{w: errOut}}
	return s.run(in)
}

type server struct {
	plugin lore.Plugin
	out    io.Writer
	errOut io.Writer
}

// Each source gets a handler of its own and the panic and unusable reports
// write errOut raw, so one lock orders every diagnostic.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

const (
	readBufferBytes = 64 << 10

	messageLimit = 256
	redaction    = "[redacted]"
	tooDeep      = "[too deeply nested]"

	maxCauseDepth = 100
	maxAttrDepth  = 100
)

func (s server) run(in io.Reader) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	requests := make(chan request)
	stop := make(chan struct{})
	defer close(stop)

	failed := make(chan error, 1)
	go func() { failed <- readRequests(in, requests, stop, cancel) }()

	for req := range requests {
		switch {
		case req.cause != nil || req.env.ID == "":
			if err := s.reportUnusable(req.env.ID, req.cause); err != nil {
				return err
			}
		default:
			err := s.respond(ctx, req.env, req.line)
			if err != nil || req.env.Op == wire.OpShutdown {
				return err
			}
		}
	}

	err := <-failed
	switch {
	case errors.Is(err, bufio.ErrTooLong):
		tooLong := fmt.Errorf("a request line exceeds the protocol's cap of %d bytes", wire.MaxLineBytes)
		_, _ = fmt.Fprintln(s.errOut, tooLong)
		return tooLong
	case err != nil:
		return fmt.Errorf("reading requests: %w", err)
	}
	return nil
}

type request struct {
	env   wire.Envelope
	line  []byte
	cause error
}

func readRequests(in io.Reader, requests chan<- request, stop <-chan struct{}, cancel context.CancelFunc) error {
	defer close(requests)
	defer cancel()

	lines := bufio.NewScanner(in)
	lines.Buffer(make([]byte, 0, readBufferBytes), wire.MaxLineBytes)

	for lines.Scan() {
		req := request{line: bytes.Clone(lines.Bytes())}
		req.cause = json.Unmarshal(req.line, &req.env)
		select {
		case requests <- req:
		case <-stop:
			return nil
		}
	}
	return lines.Err()
}

func (s server) reportUnusable(id string, cause error) error {
	message := "the request carries no id"
	if cause != nil {
		message = fmt.Sprintf("unreadable request: %v", cause)
	}
	if id == "" {
		_, _ = fmt.Fprintln(s.errOut, bounded(message))
		return nil
	}
	return s.answer(errorFrame(id, wire.KindInternal, message))
}

// The host makes one record per line, so the reason cannot carry a newline;
// the stack is left whole, its arguments printed as words of hex.
func (s server) reportPanic(op string, reason any, secrets []string) {
	text := strings.ReplaceAll(authorText(reason, secrets), "\n", " ")
	_, _ = fmt.Fprintf(s.errOut, "panic answering %q: %s\n%s", op, text, debug.Stack())
}

func requestSecrets(line []byte) []string {
	var req struct {
		Secrets map[string]string `json:"secrets"`
	}
	if json.Unmarshal(line, &req) != nil {
		return nil
	}
	return secretValues(req.Secrets)
}

func (s server) respond(ctx context.Context, env wire.Envelope, line []byte) (err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			s.reportPanic(env.Op, panicked, requestSecrets(line))
			err = s.answer(errorFrame(env.ID, wire.KindInternal,
				fmt.Sprintf("the plugin panicked answering %q; the stack is on stderr", env.Op)))
		}
	}()

	if env.V != lore.APIVersion {
		return s.answer(errorFrame(env.ID, wire.KindInternal,
			fmt.Sprintf("plugin speaks api_version %d, host speaks %d", lore.APIVersion, env.V)))
	}

	switch env.Op {
	case wire.OpManifest:
		manifest := s.plugin.Manifest()
		return s.answer(wire.Frame{V: lore.APIVersion, ID: env.ID, OK: true, Manifest: &manifest})
	case wire.OpChanges:
		return s.streamChanges(ctx, env, line)
	case wire.OpRemote:
		return s.answer(s.remoteFrame(env, line))
	case wire.OpShutdown:
		return s.answer(wire.Frame{V: lore.APIVersion, ID: env.ID, OK: true})
	}
	return s.answer(errorFrame(env.ID, wire.KindInternal, fmt.Sprintf("the plugin implements no operation %q", env.Op)))
}

func (s server) streamChanges(ctx context.Context, env wire.Envelope, line []byte) error {
	var req wire.ChangesRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return s.answer(errorFrame(env.ID, wire.KindInternal, fmt.Sprintf("unreadable %s request: %v", env.Op, err)))
	}

	cursor := req.Cursor
	if len(cursor) == 0 {
		cursor = nil
	}

	secrets := secretValues(req.Secrets)
	connector, refusal := s.openSource(env, lore.SourceConfig{
		Instance: req.Instance,
		Config:   req.Config,
		Secrets:  req.Secrets,
	}, secrets)
	if refusal != nil {
		return s.answer(*refusal)
	}

	answered := false
	defer func() {
		panicked := recover()
		switch {
		case panicked == nil:
		case !answered:
			panic(panicked)
		default:
			s.reportPanic(env.Op, panicked, secrets)
		}
	}()

	terminate := func(frame wire.Frame) error {
		err := s.answer(frame)
		answered = true
		return err
	}

	for batch, err := range connector.Changes(ctx, cursor) {
		switch {
		case err != nil:
			return terminate(failureFrame(env.ID, err, wire.KindInternal, secrets))
		case len(batch.Cursor) == 0:
			return terminate(errorFrame(env.ID, wire.KindInternal, fmt.Sprintf(
				"connector %q yielded %d documents without a cursor, so committing them would checkpoint nothing",
				connector.Name(), len(batch.Docs))))
		}

		sent, err := s.emit(batchFrame(env.ID, batch))
		if err != nil {
			return err
		}
		if !sent {
			answered = true
			return nil
		}
	}
	if cause := ctx.Err(); cause != nil {
		return terminate(failureFrame(env.ID, cause, wire.KindInternal, secrets))
	}
	return terminate(wire.Frame{V: lore.APIVersion, ID: env.ID, Done: true})
}

func (s server) remoteFrame(env wire.Envelope, line []byte) wire.Frame {
	var req wire.RemoteRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return errorFrame(env.ID, wire.KindInternal, fmt.Sprintf("unreadable %s request: %v", env.Op, err))
	}

	connector, refusal := s.openSource(env, lore.SourceConfig{
		Instance: req.Instance,
		Config:   req.Config,
		Secrets:  req.Secrets,
	}, secretValues(req.Secrets))
	if refusal != nil {
		return *refusal
	}

	matcher, ok := connector.(lore.RemoteMatcher)
	if !ok {
		return errorFrame(env.ID, wire.KindInternal, fmt.Sprintf("connector %q matches no remote", connector.Name()))
	}
	return wire.Frame{V: lore.APIVersion, ID: env.ID, OK: true, Matches: matcher.MatchesRemote(req.Remote)}
}

func (s server) openSource(env wire.Envelope, config lore.SourceConfig, secrets []string) (lore.Connector, *wire.Frame) {
	source, ok := s.plugin.(lore.SourcePlugin)
	if !ok {
		refusal := errorFrame(env.ID, wire.KindInternal, fmt.Sprintf(
			"the plugin value implements no source, so it serves no %s (kind %q)", env.Op, s.plugin.Manifest().Kind))
		return nil, &refusal
	}

	config.Host = lore.Host{Log: slog.New(redacting{
		Handler: slog.NewTextHandler(s.errOut, &slog.HandlerOptions{Level: slog.LevelDebug}),
		secrets: secrets,
	})}

	connector, err := source.NewSource(config)
	if err != nil {
		refusal := failureFrame(env.ID, err, wire.KindInvalidConfig, secrets)
		return nil, &refusal
	}
	return connector, nil
}

func (s server) answer(frame wire.Frame) error {
	_, err := s.emit(frame)
	return err
}

func (s server) emit(frame wire.Frame) (sent bool, err error) {
	line, err := json.Marshal(frame)
	switch {
	case err != nil:
		line, err = json.Marshal(errorFrame(frame.ID, wire.KindInternal,
			fmt.Sprintf("cannot encode the answer: %v", err)))
	case len(line) >= wire.MaxLineBytes:
		line, err = json.Marshal(errorFrame(frame.ID, wire.KindInternal,
			fmt.Sprintf("the answer is %d bytes and the protocol caps a frame at %d", len(line)+1, wire.MaxLineBytes)))
	default:
		sent = true
	}
	if err != nil {
		return false, fmt.Errorf("encoding the answer: %w", err)
	}

	if _, err := s.out.Write(append(line, '\n')); err != nil {
		return false, fmt.Errorf("writing the answer: %w", err)
	}
	return sent, nil
}

func errorFrame(id, kind, message string) wire.Frame {
	return wire.Frame{V: lore.APIVersion, ID: id, Error: &wire.Error{Message: bounded(message), Kind: bounded(kind)}}
}

func bounded(text string) string {
	if len(text) <= messageLimit {
		return text
	}
	return strings.ToValidUTF8(text[:messageLimit], "") + "…"
}

func batchFrame(id string, batch lore.Batch) wire.Frame {
	return wire.Frame{V: lore.APIVersion, ID: id, Batch: &wire.Batch{Docs: batch.Docs, Cursor: &batch.Cursor}}
}

func failureFrame(id string, cause error, fallback string, secrets []string) wire.Frame {
	kind := fallback
	if reported := reportedKind(cause, maxCauseDepth); reported != "" {
		kind = reported
	}
	return errorFrame(id, kind, redacted(cause.Error(), secrets))
}

func reportedKind(cause error, depth int) string {
	if depth == 0 {
		return ""
	}

	var kind string
	switch failure := cause.(type) {
	case lore.Failure:
		kind = failure.Kind
	case *lore.Failure:
		if failure == nil {
			return ""
		}
		kind = failure.Kind
	}
	if kind != "" {
		return kind
	}

	switch wrapper := cause.(type) {
	case interface{ Unwrap() error }:
		return reportedKind(wrapper.Unwrap(), depth-1)
	case interface{ Unwrap() []error }:
		for _, wrapped := range wrapper.Unwrap() {
			if kind := reportedKind(wrapped, depth-1); kind != "" {
				return kind
			}
		}
	}
	return ""
}

// Longest first, so a secret value holding another as a prefix cannot leave
// the rest of itself behind.
func secretValues(secrets map[string]string) []string {
	values := slices.SortedFunc(maps.Values(secrets), func(a, b string) int { return len(b) - len(a) })
	return slices.DeleteFunc(values, func(secret string) bool { return secret == "" })
}

func redacted(text string, secrets []string) string {
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, redaction)
	}
	return text
}

func readable(text string, secrets []string) string {
	return bounded(redacted(text, secrets))
}

type redacting struct {
	slog.Handler
	secrets []string
}

func (h redacting) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, readable(record.Message, h.secrets), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(h.attr(attr, maxAttrDepth))
		return true
	})
	return h.Handler.Handle(ctx, clean)
}

func (h redacting) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		clean[i] = h.attr(attr, maxAttrDepth)
	}
	return redacting{Handler: h.Handler.WithAttrs(clean), secrets: h.secrets}
}

func (h redacting) WithGroup(name string) slog.Handler {
	return redacting{Handler: h.Handler.WithGroup(name), secrets: h.secrets}
}

func (h redacting) attr(attr slog.Attr, depth int) slog.Attr {
	if attr.Equal(slog.Attr{}) {
		return attr
	}

	attr.Value = h.value(attr.Value, depth)
	return attr
}

// A group's leaves are author text too, so the walk goes through them; the
// depth bound holds because a LogValuer can synthesise groups without end.
func (h redacting) value(value slog.Value, depth int) slog.Value {
	if depth == 0 {
		return slog.StringValue(tooDeep)
	}

	switch value = value.Resolve(); value.Kind() {
	case slog.KindString:
		return slog.StringValue(readable(value.String(), h.secrets))
	case slog.KindGroup:
		group := slices.Clone(value.Group())
		for i, attr := range group {
			group[i] = h.attr(attr, depth-1)
		}
		return slog.GroupValue(group...)
	case slog.KindAny:
		return slog.StringValue(authorText(value.Any(), h.secrets))
	}
	return value
}

func authorText(value any, secrets []string) string {
	switch typed := value.(type) {
	case string:
		return readable(typed, secrets)
	case error:
		if message, ok := errorMessage(typed); ok {
			return readable(message, secrets)
		}
	}
	return omitted(value)
}

func errorMessage(cause error) (message string, ok bool) {
	defer func() { ok = recover() == nil }()
	return cause.Error(), true
}

func omitted(value any) string {
	return bounded(fmt.Sprintf("[value of type %T omitted]", value))
}
