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
	"math/rand/v2"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/setthasit/Lore/sdk"
)

type tuning struct {
	manifest time.Duration
	unary    time.Duration
	complete time.Duration
	idle     time.Duration
	shutdown time.Duration
	grace    time.Duration
}

func defaultTuning() tuning {
	return tuning{
		manifest: 10 * time.Second,
		unary:    60 * time.Second,
		complete: lore.CompleteTimeout,
		idle:     300 * time.Second,
		shutdown: 5 * time.Second,
		grace:    5 * time.Second,
	}
}

type session struct {
	instance string
	tuning   tuning
	log      *slog.Logger

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *stderrLog

	// Random per process so a plugin cannot pass the correlation check with ids copied from an example.
	idPrefix string
	requests int

	waitOnce sync.Once
	waitErr  error
}

const outputChunkBytes = 64 << 10

func spawn(binary, instance string, host lore.Host, tune tuning) (*session, error) {
	cmd := exec.Command(binary)
	cmd.Env = minimalEnv()
	stderr := &stderrLog{log: host.Log, instance: instance}
	cmd.Stderr = stderr
	// A grandchild that inherited stderr holds the pipe open after the plugin exits.
	cmd.WaitDelay = tune.grace

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, protocolError(instance, opManifest, nil, "cannot open the plugin's stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, protocolError(instance, opManifest, nil, "cannot open the plugin's stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, protocolError(instance, opManifest, nil, "cannot execute the plugin binary %s: %v", binary, err)
	}

	return &session{
		instance: instance,
		tuning:   tune,
		log:      host.Log,
		cmd:      cmd,
		stdin:    stdin,
		stdout:   bufio.NewReaderSize(stdout, outputChunkBytes),
		stderr:   stderr,
		idPrefix: strconv.FormatUint(rand.Uint64(), 36),
	}, nil
}

func handshake(ctx context.Context, binary, instance string, host lore.Host, tune tuning) (*session, lore.Manifest, error) {
	s, err := spawn(binary, instance, host, tune)
	if err != nil {
		return nil, lore.Manifest{}, err
	}

	env := s.begin(opManifest)
	if err := s.send(ctx, env, manifestRequest{envelope: env}, s.tuning.manifest); err != nil {
		return nil, lore.Manifest{}, err
	}
	f, err := s.await(ctx, env, s.tuning.manifest)
	if err != nil {
		return nil, lore.Manifest{}, err
	}
	if f.Manifest == nil {
		s.abort()
		return nil, lore.Manifest{}, protocolError(instance, opManifest, nil, "answered the handshake without a manifest")
	}

	manifest := *f.Manifest
	if manifest.APIVersion != lore.APIVersion {
		s.abort()
		return nil, lore.Manifest{}, protocolError(instance, opManifest, nil,
			"plugin %q speaks api_version %d, host speaks %d", manifest.Name, manifest.APIVersion, lore.APIVersion)
	}
	if manifest.Name == "" {
		s.abort()
		return nil, lore.Manifest{}, protocolError(instance, opManifest, nil, "manifest declares no name")
	}
	switch manifest.Kind {
	case lore.KindSource, lore.KindProvider, lore.KindCode:
	default:
		s.abort()
		return nil, lore.Manifest{}, protocolError(instance, opManifest, nil,
			"plugin %q declares kind %q, which is none of %q, %q, %q",
			manifest.Name, manifest.Kind, lore.KindSource, lore.KindProvider, lore.KindCode)
	}
	return s, manifest, nil
}

func (s *session) begin(op string) envelope {
	s.requests++
	return envelope{V: lore.APIVersion, ID: fmt.Sprintf("%s-%d", s.idPrefix, s.requests), Op: op}
}

// send writes one request line under a timeout: a plugin that never reads its
// stdin would otherwise hang the host once a request outgrows the pipe buffer.
func (s *session) send(ctx context.Context, env envelope, req any, timeout time.Duration) error {
	line, err := json.Marshal(req)
	if err != nil {
		s.abort()
		return protocolError(s.instance, env.Op, err, "cannot encode the %s request", env.Op)
	}
	// encoding/json escapes control characters, so a request is always exactly one line.
	line = append(line, '\n')

	done := make(chan error, 1)
	go func() {
		_, writeErr := s.stdin.Write(line)
		done <- writeErr
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case writeErr := <-done:
		if writeErr != nil {
			return s.crashed(env.Op, writeErr)
		}
		return nil
	case <-timer.C:
		s.abort()
		return protocolError(s.instance, env.Op, nil, "did not read the %s request within %s", env.Op, timeout)
	case <-ctx.Done():
		s.abort()
		return protocolError(s.instance, env.Op, ctx.Err(), "cancelled while sending %s", env.Op)
	}
}

func (s *session) await(ctx context.Context, env envelope, timeout time.Duration) (*frame, error) {
	f, err := s.read(ctx, env.Op, timeout)
	if err != nil {
		return nil, err
	}
	if f.ID != env.ID {
		s.abort()
		return nil, protocolError(s.instance, env.Op, nil,
			"answered %s with id %q, host sent id %q, so no frame can be correlated any more", env.Op, f.ID, env.ID)
	}
	// Checked before the version below, so a plugin's own rejection message is not replaced by ours.
	if f.Error != nil {
		s.endRound(ctx, env.Op)
		return nil, fromWire(s.instance, env.Op, f.Error)
	}
	if f.V != lore.APIVersion {
		s.abort()
		return nil, protocolError(s.instance, env.Op, nil,
			"answered %s with protocol version %d, host speaks %d", env.Op, f.V, lore.APIVersion)
	}
	return f, nil
}

func (s *session) endRound(ctx context.Context, op string) {
	if op == opShutdown {
		s.abort()
		return
	}
	_ = s.close(ctx)
}

func (s *session) read(ctx context.Context, op string, timeout time.Duration) (*frame, error) {
	type result struct {
		line []byte
		err  error
	}

	// Every path that abandons this goroutine kills the process, which ends the read.
	done := make(chan result, 1)
	go func() {
		line, err := s.readLine()
		done <- result{line: line, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		switch {
		case errors.Is(r.err, errLineTooLong):
			s.abort()
			return nil, protocolError(s.instance, op, nil,
				"answered %s with a line over the %d MiB limit; a batch too large to frame must be split", op, maxLineBytes>>20)
		case r.err != nil:
			return nil, s.crashed(op, r.err)
		}

		var f frame
		if err := json.Unmarshal(r.line, &f); err != nil {
			s.abort()
			return nil, protocolError(s.instance, op, err,
				"wrote a line on stdout that is not a protocol frame during %s: %s", op, excerpt(r.line))
		}
		return &f, nil
	case <-timer.C:
		s.abort()
		return nil, protocolError(s.instance, op, nil, "did not answer %s within %s", op, timeout)
	case <-ctx.Done():
		s.abort()
		return nil, protocolError(s.instance, op, ctx.Err(), "cancelled while waiting for %s", op)
	}
}

var errLineTooLong = errors.New("protocol frame exceeds the line limit")

func (s *session) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := s.stdout.ReadSlice('\n')
		if len(line)+len(chunk) > maxLineBytes {
			return nil, errLineTooLong
		}
		line = append(line, chunk...)

		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil:
			return line, err
		}
		return line[:len(line)-1], nil
	}
}

func (s *session) close(ctx context.Context) error {
	env := s.begin(opShutdown)
	if err := s.send(ctx, env, shutdownRequest{envelope: env}, s.tuning.shutdown); err != nil {
		return err
	}
	f, err := s.await(ctx, env, s.tuning.shutdown)
	if err != nil {
		return err
	}
	if !f.OK {
		s.abort()
		return protocolError(s.instance, opShutdown, nil, "answered shutdown without ok")
	}

	_ = s.stdin.Close()
	if err := s.waitWithin(s.tuning.shutdown); err != nil {
		return &crashError{instance: s.instance, op: opShutdown, detail: err.Error(), cause: err}
	}
	return nil
}

func (s *session) abort() {
	_ = s.terminate()
}

func (s *session) terminate() error {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = interrupt(s.cmd.Process)
	}
	return s.waitWithin(s.tuning.grace)
}

func (s *session) waitWithin(grace time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- s.wait() }()

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		return <-done
	}
}

func (s *session) wait() error {
	s.waitOnce.Do(func() {
		s.waitErr = s.cmd.Wait()
		if errors.Is(s.waitErr, exec.ErrWaitDelay) {
			s.log.Warn(s.instance + ": exited but left a descendant holding its output pipes; the host stopped waiting for it")
			s.waitErr = nil
		}
		s.stderr.flush()
	})
	return s.waitErr
}

func (s *session) crashed(op string, cause error) error {
	waitErr := s.terminate()

	detail := "stdout closed before the response"
	if cause != nil && !errors.Is(cause, io.EOF) {
		detail = cause.Error()
	}
	switch {
	case waitErr != nil:
		detail = fmt.Sprintf("%s (%v)", detail, waitErr)
	default:
		detail += " though the process exited 0"
	}
	return &crashError{instance: s.instance, op: op, detail: detail, cause: waitErr}
}

func excerpt(line []byte) string {
	const limit = 120
	if len(line) > limit {
		return strconv.Quote(string(line[:limit])) + "…"
	}
	return strconv.Quote(string(line))
}

type stderrLog struct {
	log      *slog.Logger
	instance string

	mu      sync.Mutex
	partial []byte
}

const maxStderrLine = outputChunkBytes

func (w *stderrLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for rest := p; len(rest) > 0; {
		end := bytes.IndexByte(rest, '\n')
		if end < 0 {
			w.partial = append(w.partial, rest...)
			if len(w.partial) >= maxStderrLine {
				w.emit()
			}
			break
		}
		w.partial = append(w.partial, rest[:end]...)
		w.emit()
		rest = rest[end+1:]
	}
	return len(p), nil
}

func (w *stderrLog) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emit()
}

func (w *stderrLog) emit() {
	if len(w.partial) == 0 {
		return
	}
	w.log.Debug(w.instance + ": " + string(w.partial))
	w.partial = w.partial[:0]
}
