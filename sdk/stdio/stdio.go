// Package stdio runs a plugin's side of the NDJSON protocol: NDJSON requests
// in, one answer per line out, diagnostics on a separate error stream. A
// request line over the protocol's cap ends the loop, because no id can be
// recovered from it.
package stdio

import (
	"bufio"
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

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/wire"
)

// Serve answers the host's requests on the process's own streams until stdin
// reaches EOF or the host asks for shutdown.
func Serve(plugin lore.Plugin) error {
	return ServeStreams(plugin, os.Stdin, os.Stdout, os.Stderr)
}

// ServeStreams is Serve over streams the caller chooses; out carries answers
// and errOut carries diagnostics and the plugin's own logs.
func ServeStreams(plugin lore.Plugin, in io.Reader, out, errOut io.Writer) error {
	s := server{
		plugin: plugin,
		out:    out,
		errOut: errOut,
		host:   lore.Host{Log: slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: slog.LevelDebug}))},
	}
	return s.run(in)
}

type server struct {
	plugin lore.Plugin
	out    io.Writer
	errOut io.Writer
	host   lore.Host
}

const (
	readBufferBytes = 64 << 10

	messageLimit = 256
	redaction    = "[redacted]"

	maxCauseDepth = 100
)

func (s server) run(in io.Reader) error {
	requests := bufio.NewScanner(in)
	requests.Buffer(make([]byte, 0, readBufferBytes), wire.MaxLineBytes)

	for requests.Scan() {
		line := requests.Bytes()

		var env wire.Envelope
		cause := json.Unmarshal(line, &env)
		if cause != nil || env.ID == "" {
			if err := s.reportUnusable(env.ID, cause); err != nil {
				return err
			}
			continue
		}

		if err := s.respond(env, line); err != nil {
			return err
		}
		if env.Op == wire.OpShutdown {
			return nil
		}
	}

	err := requests.Err()
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

func (s server) reportUnusable(id string, cause error) error {
	message := "the request carries no id"
	if cause != nil {
		message = fmt.Sprintf("unreadable request: %v", cause)
	}
	if id == "" {
		_, _ = fmt.Fprintln(s.errOut, message)
		return nil
	}
	return s.answer(errorFrame(id, wire.KindInternal, message))
}

func (s server) respond(env wire.Envelope, line []byte) (err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			_, _ = fmt.Fprintf(s.errOut, "panic answering %q: %v\n%s", env.Op, panicked, debug.Stack())
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
		return s.streamChanges(env, line)
	case wire.OpRemote:
		return s.answer(s.remoteFrame(env, line))
	case wire.OpShutdown:
		return s.answer(wire.Frame{V: lore.APIVersion, ID: env.ID, OK: true})
	}
	return s.answer(errorFrame(env.ID, wire.KindInternal, fmt.Sprintf("the plugin implements no operation %q", env.Op)))
}

func (s server) streamChanges(env wire.Envelope, line []byte) error {
	var req wire.ChangesRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return s.answer(errorFrame(env.ID, wire.KindInternal, fmt.Sprintf("unreadable %s request: %v", env.Op, err)))
	}

	cursor := req.Cursor
	if len(cursor) == 0 {
		cursor = nil
	}

	connector, refusal := s.openSource(env, lore.SourceConfig{
		Instance: req.Instance,
		Config:   req.Config,
		Secrets:  req.Secrets,
	})
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
			_, _ = fmt.Fprintf(s.errOut, "panic answering %q: %v\n%s", env.Op, panicked, debug.Stack())
		}
	}()

	terminate := func(frame wire.Frame) error {
		err := s.answer(frame)
		answered = true
		return err
	}

	for batch, err := range connector.Changes(context.Background(), cursor) {
		switch {
		case err != nil:
			return terminate(failureFrame(env.ID, err, wire.KindInternal, req.Secrets))
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
	})
	if refusal != nil {
		return *refusal
	}

	matcher, ok := connector.(lore.RemoteMatcher)
	if !ok {
		return errorFrame(env.ID, wire.KindInternal, fmt.Sprintf("connector %q matches no remote", connector.Name()))
	}
	return wire.Frame{V: lore.APIVersion, ID: env.ID, OK: true, Matches: matcher.MatchesRemote(req.Remote)}
}

func (s server) openSource(env wire.Envelope, config lore.SourceConfig) (lore.Connector, *wire.Frame) {
	source, ok := s.plugin.(lore.SourcePlugin)
	if !ok {
		refusal := errorFrame(env.ID, wire.KindInternal, fmt.Sprintf(
			"the plugin value implements no source, so it serves no %s (kind %q)", env.Op, s.plugin.Manifest().Kind))
		return nil, &refusal
	}

	config.Host = s.host
	connector, err := source.NewSource(config)
	if err != nil {
		refusal := failureFrame(env.ID, err, wire.KindInvalidConfig, config.Secrets)
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
	if batch.Docs == nil {
		batch.Docs = []lore.Document{}
	}
	return wire.Frame{V: lore.APIVersion, ID: id, Batch: &wire.Batch{Docs: batch.Docs, Cursor: &batch.Cursor}}
}

func failureFrame(id string, cause error, fallback string, secrets map[string]string) wire.Frame {
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
func redacted(message string, secrets map[string]string) string {
	values := slices.SortedFunc(maps.Values(secrets), func(a, b string) int { return len(b) - len(a) })
	for _, secret := range values {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, redaction)
		}
	}
	return message
}
