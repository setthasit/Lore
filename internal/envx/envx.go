package envx

import (
	"os"
	"regexp"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const Form = "${env:VAR}"

const NameRule = "upper-case letters, digits and underscores, not starting with a digit"

const EscapeRule = escapedOpen + " writes a literal " + open

const (
	open        = "${"
	escapedOpen = "$${"
	envOpen     = "${env:"
	envClose    = "}"
)

var namePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func ValidName(name string) bool {
	return namePattern.MatchString(name)
}

func Reference(name string) string {
	return envOpen + name + envClose
}

func Holds(raw string) bool {
	return unescapedOpen(raw) >= 0
}

func Escape(literal string) string {
	return strings.ReplaceAll(literal, open, escapedOpen)
}

// The error never quotes an expanded value, and quotes raw only up to the variable name.
func Expand(field, raw string) (string, error) {
	value, _, err := ExpandNames(field, raw)
	return value, err
}

// names lists the variables read, in order; none means raw held no expansion.
func ExpandNames(field, raw string) (value string, names []string, err error) {
	at := unescapedOpen(raw)
	if at < 0 {
		return unescape(raw), nil, nil
	}

	var out strings.Builder
	for at >= 0 {
		out.WriteString(unescape(raw[:at]))
		name, expanded, rest, err := expandOne(field, raw[at:])
		if err != nil {
			return "", nil, err
		}
		out.WriteString(expanded)
		names = append(names, name)
		raw = rest
		at = unescapedOpen(raw)
	}
	out.WriteString(unescape(raw))
	return out.String(), names, nil
}

func unescapedOpen(raw string) int {
	for i := 0; i < len(raw); i++ {
		switch {
		case strings.HasPrefix(raw[i:], escapedOpen):
			i += len(escapedOpen) - 1
		case strings.HasPrefix(raw[i:], open):
			return i
		}
	}
	return -1
}

func unescape(literal string) string {
	return strings.ReplaceAll(literal, escapedOpen, open)
}

func expandOne(field, expansion string) (name, value, rest string, err error) {
	body, isEnv := strings.CutPrefix(expansion, envOpen)
	name, rest, closed := strings.Cut(body, envClose)
	if !isEnv || !closed || !ValidName(name) {
		return "", "", "", internalerror.NewBadRequestError(field+" holds a "+open+" that is not "+Form+"; "+Form+
			" is the only accepted form, with VAR made of "+NameRule+", and "+EscapeRule, nil)
	}

	value, set := os.LookupEnv(name)
	if !set {
		return "", "", "", internalerror.NewBadRequestError(field+" expands "+name+", but "+name+" is not set", nil)
	}
	return name, value, rest, nil
}
