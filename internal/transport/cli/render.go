package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

func printfln(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// A negative age reads as "just now".
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

func indent(text, pad string) string {
	lines := strings.Split(strings.TrimRight(text, " \t\n"), "\n")
	for i, line := range lines {
		lines[i] = pad + strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}
