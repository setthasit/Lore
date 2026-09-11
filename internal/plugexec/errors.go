package plugexec

import "fmt"

type errorKind string

const (
	kindInvalidConfig errorKind = "invalid_config"
	kindAuth          errorKind = "auth"
	kindRateLimit     errorKind = "rate_limit"
	kindNotFound      errorKind = "not_found"
	kindInternal      errorKind = "internal"
)

type pluginError struct {
	instance string
	op       string
	kind     errorKind
	message  string

	cause error
}

func (e *pluginError) Error() string {
	return fmt.Sprintf("plugin instance %q: %s: %s (%s)", e.instance, e.op, e.message, e.kind)
}

func (e *pluginError) Unwrap() error { return e.cause }

type crashError struct {
	instance string
	op       string
	detail   string

	cause error
}

func (e *crashError) Error() string {
	return fmt.Sprintf("plugin instance %q crashed during %s: %s", e.instance, e.op, e.detail)
}

func (e *crashError) Unwrap() error { return e.cause }

func fromWire(instance, op string, wire *wireError) *pluginError {
	kind := errorKind(wire.Kind)
	message := wire.Message
	switch kind {
	case kindInvalidConfig, kindAuth, kindRateLimit, kindNotFound, kindInternal:
	default:
		if wire.Kind != "" {
			message = fmt.Sprintf("%s (plugin reported unknown kind %q)", message, wire.Kind)
		}
		kind = kindInternal
	}
	if message == "" {
		message = "the plugin reported an error with no message"
	}

	return &pluginError{
		instance: instance,
		op:       op,
		kind:     kind,
		message:  message,
	}
}

func protocolError(instance, op string, cause error, format string, args ...any) *pluginError {
	return &pluginError{
		instance: instance,
		op:       op,
		kind:     kindInternal,
		message:  fmt.Sprintf(format, args...),
		cause:    cause,
	}
}
