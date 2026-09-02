package cli

import (
	"errors"
	"fmt"
	"io"

	"lore/internal/errors/internalerror"
)

// The codes are stable: a script can branch on them instead of parsing stderr.
const (
	exitOK           = 0
	exitInternal     = 1
	exitBadRequest   = 2
	exitPrecondition = 3
	exitNotFound     = 4
)

// The kinds a caller can act on say everything actionable in Message; only an
// error the caller cannot act on falls back to the cause for diagnosis.
func report(w io.Writer, err error) int {
	if err == nil {
		return exitOK
	}

	message, code := err.Error(), exitInternal

	var classified *internalerror.Error
	if errors.As(err, &classified) {
		message = classified.Message
		switch classified.Kind {
		case internalerror.KindBadRequest:
			code = exitBadRequest
		case internalerror.KindPrecondition:
			code = exitPrecondition
		case internalerror.KindNotFound:
			code = exitNotFound
		default:
			message, code = classified.Error(), exitInternal
		}
	}

	_, _ = fmt.Fprintln(w, "lore: "+message)
	return code
}
