package secrets

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/setthasit/Lore/internal/envx"
)

const (
	Placeholder = "[redacted]"
	MinLength   = 8

	unnamedField = "(unnamed)"

	// An MCP tool result's text block JSON-encodes structured content that JSON-encodes a %q error.
	encodingDepth = 3
)

type Sink struct {
	mu      sync.RWMutex
	values  []string
	short   []string
	literal []string
}

// A value shorter than MinLength characters once trimmed is not scrubbed; its field is named in Notices.
func (s *Sink) Record(field, value string) {
	if s == nil {
		return
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if utf8.RuneCountInString(value) < MinLength {
		s.short = appendNew(s.short, cmp.Or(field, unnamedField))
		return
	}
	for _, form := range escapedForms(value) {
		s.values = appendNew(s.values, form)
	}
}

func (s *Sink) RecordLiteral(field, value string) {
	s.Record(field, value)
	if s == nil || strings.TrimSpace(value) == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.literal = appendNew(s.literal, cmp.Or(field, unnamedField))
}

var encodings = []func(string) string{quoteBody, jsonBody, jsonBodyUnescapedHTML}

func escapedForms(value string) []string {
	forms := []string{value}
	level := forms
	for range encodingDepth {
		first := len(forms)
		for _, form := range level {
			for _, encode := range encodings {
				forms = appendNew(forms, encode(form))
			}
		}
		level = forms[first:]
	}
	for _, form := range forms {
		forms = appendNew(forms, inertText(form))
		forms = appendNew(forms, inertLine(form))
	}
	return forms
}

func quoteBody(s string) string {
	quoted := strconv.Quote(s)
	return quoted[1 : len(quoted)-1]
}

func jsonBody(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded[1 : len(encoded)-1])
}

func jsonBodyUnescapedHTML(s string) string {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	encoded := strings.TrimSuffix(buf.String(), "\n")
	return encoded[1 : len(encoded)-1]
}

// inertText and inertLine mirror the terminal rendering of the same names in internal/transport/cli.
func inertText(s string) string {
	return escapeRunes(s, func(r rune) bool { return r != '\n' && r != '\t' && isEscapedInLine(r) })
}

func inertLine(s string) string {
	return escapeRunes(s, isEscapedInLine)
}

func isEscapedInLine(r rune) bool {
	return unicode.IsControl(r) || r == utf8.RuneError || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Bidi_Control)
}

func escapeRunes(s string, isEscaped func(rune) bool) string {
	var out strings.Builder
	out.Grow(len(s))
	for len(s) > 0 {
		r, width := utf8.DecodeRuneInString(s)
		if isEscaped(r) {
			out.WriteString(quoteBody(s[:width]))
		} else {
			out.WriteString(s[:width])
		}
		s = s[width:]
	}
	return out.String()
}

func appendNew(list []string, item string) []string {
	if slices.Contains(list, item) {
		return list
	}
	return append(list, item)
}

func (s *Sink) Scrub(text string) string {
	if s == nil {
		return text
	}

	s.mu.RLock()
	spans := s.covered(text)
	s.mu.RUnlock()
	if len(spans) == 0 {
		return text
	}

	slices.SortFunc(spans, func(a, b span) int { return cmp.Compare(a.start, b.start) })
	var out strings.Builder
	out.Grow(len(text))
	written := 0
	for i := 0; i < len(spans); {
		start, end := spans[i].start, spans[i].end
		for i++; i < len(spans) && spans[i].start <= end; i++ {
			end = max(end, spans[i].end)
		}
		out.WriteString(text[written:start])
		out.WriteString(Placeholder)
		written = end
	}
	out.WriteString(text[written:])
	return out.String()
}

type span struct{ start, end int }

func (s *Sink) covered(text string) []span {
	var spans []span
	for _, value := range s.values {
		first := len(spans)
		for from := 0; ; {
			at := strings.Index(text[from:], value)
			if at < 0 {
				break
			}
			start := from + at
			if last := len(spans) - 1; last >= first && start <= spans[last].end {
				spans[last].end = start + len(value)
			} else {
				spans = append(spans, span{start, start + len(value)})
			}
			from = start + 1
		}
	}
	return spans
}

func (s *Sink) Notices() []string {
	if s == nil {
		return nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	var notices []string
	if len(s.literal) > 0 {
		notices = append(notices, fmt.Sprintf("secrets written as literal values in the config: %s; write %s to keep a credential out of the file",
			strings.Join(s.literal, ", "), envx.Form))
	}
	if len(s.short) > 0 {
		notices = append(notices, fmt.Sprintf("secrets shorter than %d characters are not scrubbed: %s", MinLength, strings.Join(s.short, ", ")))
	}
	return notices
}
