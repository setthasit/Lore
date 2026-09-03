package transport

import (
	"errors"

	"lore/internal/errors/internalerror"
)

// Causes name hosts, paths and queries, so they stay in the server log.
const InternalErrorMessage = "internal error: see the lore server log for details"

// The message is always safe to send to a caller; an internal or unclassified
// error yields InternalErrorMessage, and the transport logs the cause itself.
func Classify(err error) (internalerror.Kind, string) {
	var classified *internalerror.Error
	if !errors.As(err, &classified) {
		return internalerror.KindUnclassified, InternalErrorMessage
	}

	switch classified.Kind {
	case internalerror.KindBadRequest, internalerror.KindNotFound, internalerror.KindPrecondition:
		return classified.Kind, classified.Message
	}

	return classified.Kind, InternalErrorMessage
}
