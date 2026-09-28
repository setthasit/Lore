package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/setthasit/Lore/sdk"
)

func (r *Repo) Blame(ctx context.Context, file string, startLine, endLine int) ([]lore.BlameSpan, error) {
	rel, err := repoRelPath(file)
	if err != nil {
		return nil, err
	}
	if startLine < 1 {
		return nil, fmt.Errorf("blame %s: start line %d is below 1", rel, startLine)
	}
	if endLine < startLine {
		// git answers a reversed -L range with lines 1..start instead of rejecting it.
		return nil, fmt.Errorf("blame %s: end line %d precedes start line %d", rel, endLine, startLine)
	}

	// Pinning HEAD keeps working-tree edits from blaming to the all-zero SHA.
	out, err := r.run(ctx, "blame", "--porcelain",
		"-L", strconv.Itoa(startLine)+","+strconv.Itoa(endLine), "HEAD", "--", rel)
	if err != nil {
		return nil, err
	}
	return parseBlamePorcelain(string(out))
}

type commitMeta struct {
	author string
	time   time.Time
}

// Porcelain repeats a commit's author block only on its first appearance, so later
// spans of the same commit are filled from meta.
func parseBlamePorcelain(out string) ([]lore.BlameSpan, error) {
	meta := make(map[string]commitMeta)
	var spans []lore.BlameSpan
	var sha string
	var line int

	for rest := out; rest != ""; {
		var text string
		text, rest, _ = strings.Cut(rest, "\n")

		if content, isContent := strings.CutPrefix(text, "\t"); isContent {
			if sha == "" {
				return nil, fmt.Errorf("git blame porcelain: content line %q precedes any commit header", text)
			}
			if n := len(spans) - 1; n >= 0 && spans[n].SHA == sha && spans[n].LineEnd+1 == line {
				spans[n].LineEnd = line
				spans[n].Lines = append(spans[n].Lines, content)
				continue
			}
			spans = append(spans, lore.BlameSpan{
				SHA:       sha,
				LineStart: line,
				LineEnd:   line,
				Author:    meta[sha].author,
				Time:      meta[sha].time,
				Lines:     []string{content},
			})
			continue
		}
		if headerSHA, finalLine, isHeader := parseBlameHeader(text); isHeader {
			sha, line = headerSHA, finalLine
			continue
		}
		if sha == "" {
			continue
		}
		key, value, _ := strings.Cut(text, " ")
		entry := meta[sha]
		switch key {
		case "author":
			entry.author = value
		case "author-time":
			when, err := epochTime(value)
			if err != nil {
				return nil, err
			}
			entry.time = when
		default:
			continue
		}
		meta[sha] = entry
	}
	return spans, nil
}

// A group header is "<hex sha> <original line> <final line> [<lines in group>]".
func parseBlameHeader(text string) (sha string, finalLine int, ok bool) {
	fields := strings.Fields(text)
	if len(fields) < 3 || !isCommitSHA(fields[0]) {
		return "", 0, false
	}
	finalLine, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, false
	}
	return fields[0], finalLine, true
}

func isCommitSHA(value string) bool {
	// A clone created with --object-format=sha256 names commits with 64 hex digits.
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
