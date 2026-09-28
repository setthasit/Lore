package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/secrets"
)

// The codes are stable: a script can branch on them instead of parsing stderr.
const (
	exitOK           = 0
	exitInternal     = 1
	exitBadRequest   = 2
	exitPrecondition = 3
	exitNotFound     = 4
)

func Report(w io.Writer, sink *secrets.Sink, err error) int {
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

	for _, line := range strings.Split(sink.Scrub(message), "\n") {
		_, _ = fmt.Fprintln(w, "lore: "+inertLine(line))
	}
	return code
}
