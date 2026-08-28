package cli

import (
	"errors"
	"fmt"
	"io"

	"lore/internal/errors/internalerror"
)

// Exit codes are the CLI's protocol mapping of internalerror kinds
// (02 — Error handling). They are stable, so a script can branch on the reason a
// command failed without parsing stderr:
//
//	0  success
//	1  internal — the operator cannot act on it
//	2  bad request — the invocation or the configuration is wrong
//	3  precondition — the workspace needs an action first (e.g. `lore sync --reembed`)
//	4  not found — the named thing does not exist
const (
	exitOK           = 0
	exitInternal     = 1
	exitBadRequest   = 2
	exitPrecondition = 3
	exitNotFound     = 4
)

// report writes err to w and returns the exit code its kind maps to.
//
// A classified error is printed as itself, dropping whatever wrapping it
// travelled through: fx reports a failed provider with its own construction
// narrative, which tells an operator nothing the message does not. The cause
// chain is kept — unlike the MCP surface, the CLI runs on the operator's own
// machine, where the underlying failure is the useful part.
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
