package plugexec

import (
	"fmt"
	"strconv"

	"github.com/setthasit/Lore/sdk/wire"
)

type errorKind string

const (
	kindInvalidConfig errorKind = wire.KindInvalidConfig
	kindAuth          errorKind = wire.KindAuth
	kindRateLimit     errorKind = wire.KindRateLimit
	kindNotFound      errorKind = wire.KindNotFound
	kindInternal      errorKind = wire.KindInternal
)

type pluginError struct {
	instance string
	op       string
	kind     errorKind
	message  string

	cause error
}

func (e *pluginError) Error() string {
	return fmt.Sprintf("plugin instance %s: %s: %s (%s)", Excerpt(e.instance), e.op, e.message, e.kind)
}

func (e *pluginError) Unwrap() error { return e.cause }

type crashError struct {
	instance string
	op       string
	detail   string

	cause error
}

func (e *crashError) Error() string {
	return fmt.Sprintf("plugin instance %s crashed during %s: %s", Excerpt(e.instance), e.op, e.detail)
}

func (e *crashError) Unwrap() error { return e.cause }

func fromWire(instance, op string, reported *wire.Error) *pluginError {
	kind := errorKind(reported.Kind)
	message := ""
	if reported.Message != "" {
		message = Excerpt(reported.Message)
	}
	switch kind {
	case kindInvalidConfig, kindAuth, kindRateLimit, kindNotFound, kindInternal:
	default:
		if reported.Kind != "" {
			message = fmt.Sprintf("%s (plugin reported unknown kind %s)", message, Excerpt(reported.Kind))
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

const excerptLimit = 120

func Excerpt(text string) string {
	if len(text) > excerptLimit {
		return strconv.Quote(text[:excerptLimit]) + "…"
	}
	return strconv.Quote(text)
}
