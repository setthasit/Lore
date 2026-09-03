package sqlite

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/setthasit/Lore/internal/entities"
)

// Shortest abbreviation a SHA is quoted by in prose, and the full SHA-1 width.
const (
	minSHARefLen = 7
	maxSHARefLen = 40
)

const docIDSeparators = 2

var (
	ticketKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]+-[0-9]+$`)
	numberRefRe = regexp.MustCompile(`^(?:([A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*))?#?([0-9]+)$`)
)

const resolveRefSQL = `SELECT ` + documentMetaColumns + ` FROM documents WHERE `

// Every shape the ref could be is queried, because some strings are legitimately
// two shapes at once — "1234567" is both an abbreviated SHA and an issue number.
func (s *Store) ResolveRef(ctx context.Context, ref string) ([]entities.DocumentMeta, error) {
	ref = strings.TrimSpace(ref)
	clauses, args := refClauses(ref)
	if len(clauses) == 0 {
		return nil, nil
	}

	branches := make([]string, len(clauses))
	for i, clause := range clauses {
		branches[i] = resolveRefSQL + clause
	}

	query := strings.Join(branches, " UNION ") + " ORDER BY doc_id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: resolve ref %q: %w", ref, err)
	}
	defer func() { _ = rows.Close() }()

	metas, err := scanDocumentMetas(rows, len(clauses))
	if err != nil {
		return nil, fmt.Errorf("sqlite: resolve ref %q: %w", ref, err)
	}
	return metas, nil
}

func refClauses(ref string) ([]string, []any) {
	var (
		clauses []string
		args    []any
	)
	add := func(clause string, values ...any) {
		clauses = append(clauses, clause)
		args = append(args, values...)
	}

	isURL := strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
	switch {
	case isURL:
		add(`url = ?`, ref)
	case strings.Count(ref, ":") >= docIDSeparators:
		add(`doc_id = ?`, ref)
	}

	if sha, ok := shaRef(ref); ok {
		if len(sha) >= shaPrefixLen {
			add(`sha_prefix = ?`, sha[:shaPrefixLen])
		} else {
			add(`sha_prefix >= ? AND sha_prefix < ?`, sha, upperBound(sha))
		}
	}

	if ticketKeyRe.MatchString(ref) {
		add(`external_key = ?`, ref)
	}

	if m := numberRefRe.FindStringSubmatch(ref); m != nil {
		clause, values := numberRefClause(m[1], m[2])
		add(clause, values...)
	}
	return clauses, args
}

func numberRefClause(slug, number string) (string, []any) {
	if slug != "" {
		return `external_key IN (?, ?)`, []any{slug + "/pull/" + number, slug + "/issues/" + number}
	}
	// Scans: a bare number names no repo, and documents(source, type) has no type-only prefix.
	clause := `type IN (?, ?) AND (external_key LIKE '%/pull/' || ?` +
		` OR external_key LIKE '%/issues/' || ?)`
	return clause, []any{string(entities.DocTypePR), string(entities.DocTypeIssue), number, number}
}

func shaRef(ref string) (string, bool) {
	if len(ref) < minSHARefLen || len(ref) > maxSHARefLen || !isHexString(ref) {
		return "", false
	}
	return strings.ToLower(ref), true
}

func isHexString(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// Callers pass a non-empty hex prefix, so the last byte can never overflow.
func upperBound(prefix string) string {
	b := []byte(prefix)
	b[len(b)-1]++
	return string(b)
}
