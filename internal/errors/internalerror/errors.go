package internalerror

import (
	"errors"
	"fmt"
)

// Kind classifies an error so a transport can map it to a protocol-native code
// without inspecting messages.
type Kind int

const (
	// KindUnclassified marks an error that never passed through this package.
	KindUnclassified Kind = iota
	KindBadRequest
	KindNotFound
	KindPrecondition
	KindInternal
)

func (k Kind) String() string {
	switch k {
	case KindBadRequest:
		return "bad_request"
	case KindNotFound:
		return "not_found"
	case KindPrecondition:
		return "precondition"
	case KindInternal:
		return "internal"
	default:
		return "unclassified"
	}
}

// Error is a classified error carrying a caller-facing message and an optional
// cause. The cause is reachable through errors.Unwrap, errors.Is and errors.As.
type Error struct {
	Kind    Kind
	Message string

	cause error
}

func (e *Error) Error() string {
	if e.cause == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.cause)
}

func (e *Error) Unwrap() error {
	return e.cause
}

// NewBadRequestError reports malformed or invalid caller input. cause may be nil.
func NewBadRequestError(message string, cause error) error {
	return &Error{Kind: KindBadRequest, Message: message, cause: cause}
}

// NewNotFoundError reports a requested entity that does not exist. cause may be nil.
func NewNotFoundError(message string, cause error) error {
	return &Error{Kind: KindNotFound, Message: message, cause: cause}
}

// NewPreconditionError reports a workspace or state requirement the caller must
// satisfy before the operation can run. cause may be nil.
func NewPreconditionError(message string, cause error) error {
	return &Error{Kind: KindPrecondition, Message: message, cause: cause}
}

// NewInternalError reports a failure the caller cannot act on. cause may be nil.
func NewInternalError(message string, cause error) error {
	return &Error{Kind: KindInternal, Message: message, cause: cause}
}

// KindOf returns the classification of the outermost classified error in err's
// chain, or KindUnclassified when the chain holds none.
func KindOf(err error) Kind {
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Kind
	}
	return KindUnclassified
}

func IsBadRequest(err error) bool {
	return KindOf(err) == KindBadRequest
}

func IsNotFound(err error) bool {
	return KindOf(err) == KindNotFound
}

func IsPrecondition(err error) bool {
	return KindOf(err) == KindPrecondition
}

func IsInternal(err error) bool {
	return KindOf(err) == KindInternal
}
