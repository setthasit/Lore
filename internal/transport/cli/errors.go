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

func report(w io.Writer, err error) int {
	if err == nil {
		return exitOK
	}

	message, code := err.Error(), exitInternal

	var classified *internalerror.Error
	if errors.As(err, &classified) {
		message = classified.Error()
		switch classified.Kind {
		case internalerror.KindBadRequest:
			code = exitBadRequest
		case internalerror.KindPrecondition:
			code = exitPrecondition
		case internalerror.KindNotFound:
			code = exitNotFound
		default:
			code = exitInternal
		}
	}

	_, _ = fmt.Fprintln(w, "lore: "+message)
	return code
}
