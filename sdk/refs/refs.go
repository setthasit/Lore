package refs

import (
	"regexp"
	"strings"

	"github.com/setthasit/Lore/sdk"
)

var (
	ticketKeyPattern = regexp.MustCompile(`[A-Z][A-Z0-9]+-\d+`)

	// Bracketing characters stay out of the match so a markdown link yields a bare URL.
	urlPattern = regexp.MustCompile(`https?://[^\s<>()\[\]{}"'` + "`" + `]+`)

	// commitSHAPattern matches abbreviated and full lowercase hex SHAs.
	commitSHAPattern = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)

	filePathPattern = regexp.MustCompile(`[\w.-]+(?:/[\w.-]+)+\.[A-Za-z][A-Za-z0-9]*`)
)

// urlTrailing is punctuation that ends a sentence rather than a URL.
const urlTrailing = ".,;:!?"

// Set drops duplicates while keeping first-seen order.
type Set struct {
	seen map[lore.RawRef]struct{}
	refs []lore.RawRef
}

func (s *Set) Add(kind lore.RefKind, value string) {
	if value == "" {
		return
	}
	ref := lore.RawRef{Kind: kind, Value: value}
	if _, ok := s.seen[ref]; ok {
		return
	}
	if s.seen == nil {
		s.seen = make(map[lore.RawRef]struct{}, 8)
	}
	s.seen[ref] = struct{}{}
	s.refs = append(s.refs, ref)
}

func (s *Set) AddAll(kind lore.RefKind, values []string) {
	for _, v := range values {
		s.Add(kind, v)
	}
}

func (s *Set) AddTicketKeys(text string) {
	for _, m := range ticketKeyPattern.FindAllString(text, -1) {
		s.Add(lore.RefKindTicketKey, m)
	}
}

func (s *Set) AddURLs(text string) {
	for _, m := range urlPattern.FindAllString(text, -1) {
		s.Add(lore.RefKindURL, strings.TrimRight(m, urlTrailing))
	}
}

func (s *Set) AddCommitSHAs(text string) {
	for _, m := range commitSHAPattern.FindAllString(text, -1) {
		s.Add(lore.RefKindCommitSHA, m)
	}
}

func (s *Set) AddFilePaths(text string) {
	// A URL's path is not a workspace path, and its host would leak into the match.
	masked := urlPattern.ReplaceAllLiteralString(text, " ")
	for _, m := range filePathPattern.FindAllString(masked, -1) {
		s.Add(lore.RefKindFilePath, m)
	}
}

func (s *Set) Refs() []lore.RawRef { return s.refs }
