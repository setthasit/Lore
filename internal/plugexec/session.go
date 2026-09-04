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

// tuning holds the protocol's timeouts as data so the tests can shrink them
// without waiting out a five-minute idle window. The values in defaultTuning
// are the protocol's; nothing outside this package can change them.
type tuning struct {
	manifest time.Duration
	unary    time.Duration // embed, blame, log, has_file
	complete time.Duration
	idle     time.Duration // changes has no total limit, only an idle one
	shutdown time.Duration
	grace    time.Duration // between SIGTERM and SIGKILL
}

func defaultTuning() tuning {
	return tuning{
		manifest: 10 * time.Second,
		unary:    60 * time.Second,
		complete: lore.CompleteTimeout,
		// A long backfill is legitimate, a silent process is not, so the changes
		// budget is per frame rather than per stream.
		idle:     300 * time.Second,
		shutdown: 5 * time.Second,
		grace:    5 * time.Second,
	}
}

// session is one plugin process. The protocol allows one in-flight request per
// process — concurrency is the host's job, and it gets it by running more
// processes — so a session needs no request table and no scheduler.
type session struct {
	instance string
	tuning   tuning
	log      *slog.Logger

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	// idPrefix is random per process so a plugin cannot pass the correlation
	// check by hardcoding the ids it saw in an example.
	idPrefix string
	requests int

	waitOnce sync.Once
	waitErr  error
}

func spawn(binary, instance string, host lore.Host, tune tuning) (*session, error) {
	log := host.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	cmd := exec.Command(binary)
	// The child gets no inherited environment: secrets travel in the request
	// payload, so a plugin sees only what its manifest declared and cannot read
	// another plugin's token out of the process it happens to be started from.
	cmd.Env = minimalEnv()
	cmd.Stderr = &stderrLog{log: log, instance: instance}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, protocolError(instance, opManifest, "cannot open the plugin's stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, protocolError(instance, opManifest, "cannot open the plugin's stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, protocolError(instance, opManifest, "cannot execute the plugin binary %s: %v", binary, err)
	}

	return &session{
		instance: instance,
		tuning:   tune,
		log:      log,
		cmd:      cmd,
		stdin:    stdin,
		stdout:   bufio.NewReaderSize(stdout, 64<<10),
		idPrefix: strconv.FormatUint(rand.Uint64(), 36),
	}, nil
}

// handshake spawns the process and performs the one request that must come
// first on every process, so no operation can run over a contract the two sides
// have not agreed on.
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
		return nil, lore.Manifest{}, protocolError(instance, opManifest, "answered the handshake without a manifest")
	}

	manifest := *f.Manifest
	if manifest.APIVersion != lore.APIVersion {
		s.abort()
		return nil, lore.Manifest{}, protocolError(instance, opManifest,
			"plugin %q speaks api_version %d, host speaks %d", manifest.Name, manifest.APIVersion, lore.APIVersion)
	}
	if manifest.Name == "" {
		s.abort()
		return nil, lore.Manifest{}, protocolError(instance, opManifest, "manifest declares no name")
	}
	switch manifest.Kind {
	case lore.KindSource, lore.KindProvider, lore.KindCode:
	default:
		s.abort()
		return nil, lore.Manifest{}, protocolError(instance, opManifest,
			"plugin %q declares kind %q, which is none of %q, %q, %q",
			manifest.Name, manifest.Kind, lore.KindSource, lore.KindProvider, lore.KindCode)
	}
	return s, manifest, nil
}

func (s *session) begin(op string) envelope {
	s.requests++
	return envelope{V: lore.APIVersion, ID: fmt.Sprintf("%s-%d", s.idPrefix, s.requests), Op: op}
}

// send writes one request line. The write is bounded because a plugin that
// never reads its stdin would otherwise hang the host once a request outgrows
// the pipe buffer.
func (s *session) send(ctx context.Context, env envelope, req any, timeout time.Duration) error {
	line, err := json.Marshal(req)
	if err != nil {
		s.abort()
		return wrapProtocolError(s.instance, env.Op, err, "cannot encode the %s request", env.Op)
	}
	// encoding/json escapes control characters inside strings, so a request is
	// always exactly one line however a secret or a document body is spelled.
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
		return protocolError(s.instance, env.Op, "did not read the %s request within %s", env.Op, timeout)
	case <-ctx.Done():
		s.abort()
		return wrapProtocolError(s.instance, env.Op, ctx.Err(), "cancelled while sending %s", env.Op)
	}
}

// await reads the next frame for env and applies the envelope rules. An error
// frame is returned as the plugin's error and leaves the process alive and
// ready for the next request; anything malformed kills the session, because
// stdout is protocol-only and there is no resynchronization point to look for.
func (s *session) await(ctx context.Context, env envelope, timeout time.Duration) (*frame, error) {
	f, err := s.read(ctx, env.Op, timeout)
	if err != nil {
		return nil, err
	}
	if f.ID != env.ID {
		s.abort()
		return nil, protocolError(s.instance, env.Op,
			"answered %s with id %q, host sent id %q, so no frame can be correlated any more", env.Op, f.ID, env.ID)
	}
	// The error frame is read before the version check: a plugin that rejects
	// the host's protocol version answers with an error, and hiding that message
	// behind a version complaint of our own would lose the one detail it carries.
	if f.Error != nil {
		return nil, fromWire(s.instance, env.Op, f.Error)
	}
	if f.V != lore.APIVersion {
		s.abort()
		return nil, protocolError(s.instance, env.Op,
			"answered %s with protocol version %d, host speaks %d", env.Op, f.V, lore.APIVersion)
	}
	return f, nil
}

func (s *session) read(ctx context.Context, op string, timeout time.Duration) (*frame, error) {
	type result struct {
		line []byte
		err  error
	}

	// The read runs in a goroutine so the timeout and the cancellation are
	// selectable; it is one goroutine per frame and it always finishes, because
	// every path that abandons it kills the process, which ends the read.
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
			return nil, protocolError(s.instance, op,
				"answered %s with a line over the %d MiB limit; a batch too large to frame must be split", op, maxLineBytes>>20)
		case r.err != nil:
			return nil, s.crashed(op, r.err)
		}

		var f frame
		if err := json.Unmarshal(r.line, &f); err != nil {
			s.abort()
			return nil, wrapProtocolError(s.instance, op, err,
				"wrote a line on stdout that is not a protocol frame during %s: %s", op, excerpt(r.line))
		}
		return &f, nil
	case <-timer.C:
		s.abort()
		return nil, protocolError(s.instance, op, "did not answer %s within %s", op, timeout)
	case <-ctx.Done():
		s.abort()
		return nil, wrapProtocolError(s.instance, op, ctx.Err(), "cancelled while waiting for %s", op)
	}
}

var errLineTooLong = errors.New("protocol frame exceeds the line limit")

// readLine returns one NDJSON frame without its terminator. It reads through
// bufio in chunks rather than into an unbounded buffer so a plugin cannot make
// the host allocate more than the protocol's line cap.
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

// close is the ordered end of a round: the plugin answers shutdown, flushes its
// stdout and exits 0. Anything else is a crash, because the process was asked
// to leave and did not.
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
		return protocolError(s.instance, opShutdown, "answered shutdown without ok")
	}

	// Closing stdin is the plugin's cancel signal, and after shutdown there is
	// nothing left to send: a plugin that ignores its answer and lingers is
	// escalated exactly as a cancellation is.
	_ = s.stdin.Close()
	if err := s.waitWithin(s.tuning.shutdown); err != nil {
		return &CrashError{Instance: s.instance, Op: opShutdown, Detail: err.Error(), cause: err}
	}
	return nil
}

// abort is the cancellation escalation: stdin EOF, then SIGTERM, then SIGKILL
// after the grace. Its error is deliberately dropped — every caller already has
// the failure it is reporting, and how a plugin died while being killed adds
// nothing to it.
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
	s.waitOnce.Do(func() { s.waitErr = s.cmd.Wait() })
	return s.waitErr
}

// crashed reports a process that died mid-operation. The exit status is read
// first, because "exit status 3" is the whole diagnosis and a plugin author
// looking at a bare EOF has nothing to work with.
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
	return &CrashError{Instance: s.instance, Op: op, Detail: detail, cause: waitErr}
}

// excerpt keeps a malformed line quotable in an error message without pasting a
// megabyte of a plugin's stray output into a log.
func excerpt(line []byte) string {
	const limit = 120
	if len(line) > limit {
		return strconv.Quote(string(line[:limit])) + "…"
	}
	return strconv.Quote(string(line))
}

// stderrLog forwards the plugin's only diagnostic channel to the host logger at
// debug level. It splits on newlines so one plugin log line is one host log
// line, and caps a partial line so a plugin that never emits a newline cannot
// grow the host's heap.
type stderrLog struct {
	log      *slog.Logger
	instance string
	partial  []byte
}

const maxStderrLine = 64 << 10

var newline = []byte{'\n'}

func (w *stderrLog) Write(p []byte) (int, error) {
	for rest := p; len(rest) > 0; {
		line, tail, found := bytes.Cut(rest, newline)
		w.partial = append(w.partial, line...)
		if !found {
			if len(w.partial) >= maxStderrLine {
				w.emit()
			}
			break
		}
		w.emit()
		rest = tail
	}
	return len(p), nil
}

func (w *stderrLog) emit() {
	if len(w.partial) == 0 {
		return
	}
	// The instance goes in the message rather than in an attribute because the
	// registry has already tagged this logger with it, and a duplicated key
	// reads worse than the prefix the protocol asks for.
	w.log.Debug(w.instance + ": " + string(w.partial))
	w.partial = w.partial[:0]
}
