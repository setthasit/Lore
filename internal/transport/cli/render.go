package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// printfln writes one line. Write errors are dropped on purpose: the only
// writers here are stdout and a test buffer, and a command that cannot print
// has nothing better to report than what it was already printing.
func printfln(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// humanizeAge renders how long ago something happened, coarsely. A status line
// answers "is this stale?", which never needs sub-second precision — and a
// timestamp is printed alongside for anyone who wants the exact moment.
//
// A negative age means the stored timestamp is in this machine's future (a clock
// that moved, or a workspace file shared across machines) and reads as "just
// now" rather than as a nonsensical negative duration.
func humanizeAge(d time.Duration) string {
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

// indent prefixes every line of text with pad, so a multi-line excerpt stays
// visibly attached to the node it belongs to. Trailing blank lines go: they are
// an artefact of where the chunk was cut, not part of the evidence.
func indent(text, pad string) string {
	lines := strings.Split(strings.TrimRight(text, " \t\n"), "\n")
	for i, line := range lines {
		lines[i] = pad + strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}
