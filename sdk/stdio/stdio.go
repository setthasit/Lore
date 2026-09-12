// Package stdio runs a plugin's side of the NDJSON protocol: NDJSON requests
// in, one answer per line out, diagnostics on a separate error stream. A
// request line over the protocol's cap ends the loop, because no id can be
// recovered from it.
package stdio

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"

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

const readBufferBytes = 64 << 10

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

		if err := s.emit(s.answer(env, line)); err != nil {
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
	return s.emit(errorFrame(id, wire.KindInternal, message))
}

func (s server) answer(env wire.Envelope, line []byte) (frame wire.Frame) {
	defer func() {
		if panicked := recover(); panicked != nil {
			_, _ = fmt.Fprintf(s.errOut, "panic answering %q: %v\n%s", env.Op, panicked, debug.Stack())
			frame = errorFrame(env.ID, wire.KindInternal,
				fmt.Sprintf("the plugin panicked answering %q; the stack is on stderr", env.Op))
		}
	}()

	if env.V != lore.APIVersion {
		return errorFrame(env.ID, wire.KindInternal,
			fmt.Sprintf("plugin speaks api_version %d, host speaks %d", lore.APIVersion, env.V))
	}

	switch env.Op {
	case wire.OpManifest:
		manifest := s.plugin.Manifest()
		return wire.Frame{V: lore.APIVersion, ID: env.ID, OK: true, Manifest: &manifest}
	case wire.OpShutdown:
		return wire.Frame{V: lore.APIVersion, ID: env.ID, OK: true}
	case wire.OpRemote:
		return s.answerRemote(env, line)
	}
	return errorFrame(env.ID, wire.KindInternal, fmt.Sprintf("the plugin implements no operation %q", env.Op))
}

func (s server) answerRemote(env wire.Envelope, line []byte) wire.Frame {
	var req wire.RemoteRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return errorFrame(env.ID, wire.KindInternal, fmt.Sprintf("unreadable %s request: %v", env.Op, err))
	}

	source, ok := s.plugin.(lore.SourcePlugin)
	if !ok {
		return errorFrame(env.ID, wire.KindInternal, fmt.Sprintf(
			"the plugin value implements no source, so it serves no %s (kind %q)", env.Op, s.plugin.Manifest().Kind))
	}

	connector, err := source.NewSource(lore.SourceConfig{
		Instance: req.Instance,
		Config:   req.Config,
		Secrets:  req.Secrets,
		Host:     s.host,
	})
	if err != nil {
		return errorFrame(env.ID, wire.KindInvalidConfig, err.Error())
	}

	matcher, ok := connector.(lore.RemoteMatcher)
	if !ok {
		return errorFrame(env.ID, wire.KindInternal, fmt.Sprintf("connector %q matches no remote", connector.Name()))
	}
	return wire.Frame{V: lore.APIVersion, ID: env.ID, OK: true, Matches: matcher.MatchesRemote(req.Remote)}
}

func (s server) emit(frame wire.Frame) error {
	line, err := json.Marshal(frame)
	switch {
	case err != nil:
		line, err = json.Marshal(errorFrame(frame.ID, wire.KindInternal,
			fmt.Sprintf("cannot encode the answer: %v", err)))
	case len(line) >= wire.MaxLineBytes:
		line, err = json.Marshal(errorFrame(frame.ID, wire.KindInternal,
			fmt.Sprintf("the answer is %d bytes and the protocol caps a frame at %d", len(line)+1, wire.MaxLineBytes)))
	}
	if err != nil {
		return fmt.Errorf("encoding the answer: %w", err)
	}

	if _, err := s.out.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("writing the answer: %w", err)
	}
	return nil
}

func errorFrame(id, kind, message string) wire.Frame {
	return wire.Frame{V: lore.APIVersion, ID: id, Error: &wire.Error{Message: message, Kind: kind}}
}
