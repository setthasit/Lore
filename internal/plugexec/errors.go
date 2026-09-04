package plugexec

import (
	"errors"
	"fmt"
)

// ErrorKind is the classification a plugin puts on an error frame. It decides
// what the host does next, which is why an unknown kind is not passed through:
// scheduling on a kind nobody implements would silently become "retry forever".
type ErrorKind string

const (
	// KindInvalidConfig and KindAuth fail the instance immediately: neither a
	// wrong configuration nor a bad credential fixes itself, so a retry only
	// spends the operator's rate limit on a certain failure.
	KindInvalidConfig ErrorKind = "invalid_config"
	KindAuth          ErrorKind = "auth"

	// KindRateLimit is backed off and resumed from the last committed cursor.
	KindRateLimit ErrorKind = "rate_limit"

	KindNotFound ErrorKind = "not_found"
	KindInternal ErrorKind = "internal"
)

// Error is a failure the host attributes to one plugin instance: either an
// error frame the plugin sent, or a protocol violation the host detected. Both
// fail the instance the same way, and a failing instance never stops the others.
type Error struct {
	Instance string
	Op       string
	Kind     ErrorKind
	Message  string

	// Retryable is authoritative for scheduling whatever the kind, because a
	// plugin knows which of its failures are transient and the host does not.
	Retryable bool

	cause error
}

func (e *Error) Error() string {
	return fmt.Sprintf("plugin instance %q: %s: %s (%s)", e.Instance, e.Op, e.Message, e.Kind)
}

func (e *Error) Unwrap() error { return e.cause }

// CrashError reports a process that died: a non-zero exit is never a business
// error, because every expected failure — bad credentials, missing resource,
// throttling — is an error frame the process survives. It names the instance
// and the last op so the report says which work was lost.
type CrashError struct {
	Instance string
	Op       string

	// Detail is how the process died — "exit status 3", "stdout closed before
	// the response" — because that is the whole of what a plugin author has to
	// go on when a binary of theirs dies under someone else's host.
	Detail string

	cause error
}

func (e *CrashError) Error() string {
	return fmt.Sprintf("plugin instance %q crashed during %s: %s", e.Instance, e.Op, e.Detail)
}

func (e *CrashError) Unwrap() error { return e.cause }

// Retryable reports whether err is worth backing off and resuming from the last
// committed cursor. A crash is not: the process is gone and nothing says the
// next one would get further.
func Retryable(err error) bool {
	var pluginErr *Error
	if errors.As(err, &pluginErr) {
		return pluginErr.Retryable
	}
	return false
}

// fromWire turns an error frame into the host's error, applying the two rules
// the protocol states: an unknown kind is treated as internal, and rate_limit
// implies retryable even when the plugin did not say so.
func fromWire(instance, op string, wire *wireError) *Error {
	kind := ErrorKind(wire.Kind)
	message := wire.Message
	switch kind {
	case KindInvalidConfig, KindAuth, KindRateLimit, KindNotFound, KindInternal:
	default:
		// The unknown kind stays in the message: the operator needs to see what
		// the plugin actually claimed to report it to its author.
		if wire.Kind != "" {
			message = fmt.Sprintf("%s (plugin reported unknown kind %q)", message, wire.Kind)
		}
		kind = KindInternal
	}
	if message == "" {
		message = "the plugin reported an error with no message"
	}

	return &Error{
		Instance:  instance,
		Op:        op,
		Kind:      kind,
		Message:   message,
		Retryable: wire.Retryable || kind == KindRateLimit,
	}
}

// protocolError is a violation the host detected rather than one the plugin
// reported. It is internal and never retryable: a plugin that broke the framing
// once will break it again on the next round.
func protocolError(instance, op string, format string, args ...any) *Error {
	return &Error{
		Instance: instance,
		Op:       op,
		Kind:     KindInternal,
		Message:  fmt.Sprintf(format, args...),
	}
}

func wrapProtocolError(instance, op string, cause error, format string, args ...any) *Error {
	err := protocolError(instance, op, format, args...)
	err.cause = cause
	return err
}
