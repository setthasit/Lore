package secrets

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	Placeholder = "[redacted]"
	MinLength   = 8

	unnamedField = "(unnamed)"
)

type Sink struct {
	mu     sync.RWMutex
	values []string
	short  []string
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
	s.values = appendNew(s.values, value)
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
	if len(s.short) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("secrets shorter than %d characters are not scrubbed: %s", MinLength, strings.Join(s.short, ", "))}
}
