package refscan

import (
	"regexp"
	"strings"

	"lore/internal/entities"
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
	seen map[entities.RawRef]struct{}
	refs []entities.RawRef
}

func (s *Set) Add(kind entities.RefKind, value string) {
	if value == "" {
		return
	}
	ref := entities.RawRef{Kind: kind, Value: value}
	if _, ok := s.seen[ref]; ok {
		return
	}
	if s.seen == nil {
		s.seen = make(map[entities.RawRef]struct{}, 8)
	}
	s.seen[ref] = struct{}{}
	s.refs = append(s.refs, ref)
}

func (s *Set) AddAll(kind entities.RefKind, values []string) {
	for _, v := range values {
		s.Add(kind, v)
	}
}

func (s *Set) AddTicketKeys(text string) {
	for _, m := range ticketKeyPattern.FindAllString(text, -1) {
		s.Add(entities.RefKindTicketKey, m)
	}
}

func (s *Set) AddURLs(text string) {
	for _, m := range urlPattern.FindAllString(text, -1) {
		s.Add(entities.RefKindURL, strings.TrimRight(m, urlTrailing))
	}
}

func (s *Set) AddCommitSHAs(text string) {
	for _, m := range commitSHAPattern.FindAllString(text, -1) {
		s.Add(entities.RefKindCommitSHA, m)
	}
}

func (s *Set) AddFilePaths(text string) {
	// A URL's path is not a workspace path, and its host would leak into the match.
	masked := urlPattern.ReplaceAllLiteralString(text, " ")
	for _, m := range filePathPattern.FindAllString(masked, -1) {
		s.Add(entities.RefKindFilePath, m)
	}
}

func (s *Set) Refs() []entities.RawRef { return s.refs }
