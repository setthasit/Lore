package refs

import (
	"regexp"
	"slices"
	"strings"

	"github.com/setthasit/Lore/sdk"
)

var (
	ticketKeyPattern = regexp.MustCompile(`[A-Z][A-Z0-9]+-\d+`)

	// Bracketing characters stay out of the match so a markdown link yields a bare URL.
	urlPattern       = regexp.MustCompile(`https?://[^\s<>()\[\]{}"'` + "`" + `]+`)
	commitSHAPattern = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)

	filePathPattern = regexp.MustCompile(`[\w.-]+(?:/[\w.-]+)+\.[A-Za-z][A-Za-z0-9]*`)
)

// urlTrailing is punctuation that ends a sentence rather than a URL.
const urlTrailing = ".,;:!?"

// A scoped reference in a Set supersedes the same kind and value unscoped,
// whichever of the two arrives first.
type Set struct {
	seen   map[lore.RawRef]struct{}
	scoped map[lore.RawRef]struct{}
	refs   []lore.RawRef
}

func (s *Set) Add(kind lore.RefKind, value string) {
	s.AddScoped(kind, value, "")
}

func (s *Set) AddScoped(kind lore.RefKind, value, instance string) {
	if value == "" {
		return
	}
	bare := lore.RawRef{Kind: kind, Value: value}
	if _, ok := s.scoped[bare]; ok && instance == "" {
		return
	}
	ref := bare
	ref.Instance = instance
	if _, ok := s.seen[ref]; ok {
		return
	}
	if s.seen == nil {
		s.seen = make(map[lore.RawRef]struct{}, 8)
	}
	s.seen[ref] = struct{}{}
	if instance != "" {
		if s.scoped == nil {
			s.scoped = make(map[lore.RawRef]struct{}, 8)
		}
		s.scoped[bare] = struct{}{}
		if _, ok := s.seen[bare]; ok {
			delete(s.seen, bare)
			s.refs = slices.DeleteFunc(s.refs, func(r lore.RawRef) bool { return r == bare })
		}
	}
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

func (s *Set) Refs() []lore.RawRef { return slices.Clone(s.refs) }
