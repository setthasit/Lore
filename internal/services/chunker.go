package services

import (
	"strings"
	"unicode/utf8"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/sdk"
)

type Chunker interface {
	// Chunk returns doc's chunks in body order, Ordinal 0-based; a blank body yields none.
	Chunk(doc lore.Document) []entities.Chunk
}

// Chunk sizes are estimated tokens at len(text)/4 bytes per token, the BPE rule of thumb.
const (
	bytesPerToken  = 4
	minChunkTokens = 300 // below this a markdown heading is not worth breaking on
	maxChunkTokens = 500 // hard ceiling for a chunk's own content
	overlapTokens  = 50  // context carried from the previous chunk
)

const (
	minChunkBytes = minChunkTokens * bytesPerToken
	maxChunkBytes = maxChunkTokens * bytesPerToken
	overlapBytes  = overlapTokens * bytesPerToken
)

// blockSeparator joins blocks, and separates heading path, carried overlap and a chunk's own content.
const blockSeparator = "\n\n"

const threadSeparator = "#"

const wordBreaks = " \t\n"

const maxHeadingLevel = 6

const minFenceRun = 3

// chunkFormat changes whenever chunk text for an unchanged document would change,
// because a stored index split by another format is refused until rebuilt.
const chunkFormat = "2"

type chunker struct{}

var _ Chunker = chunker{}

func NewChunker() Chunker { return chunker{} }

func (chunker) Chunk(doc lore.Document) []entities.Chunk {
	body := strings.TrimSpace(doc.Body)
	if body == "" {
		return nil
	}

	switch doc.Type {
	case lore.DocTypeCommit:
		return []entities.Chunk{chunkOf(doc, 0, body, "")}
	case lore.DocTypeReviewComment, lore.DocTypeIssueComment, lore.DocTypeTicketComment:
		return []entities.Chunk{chunkOf(doc, 0, body, threadID(doc.ID))}
	default:
		return splitBody(doc, body)
	}
}

// A comment DocID is "<thread>#<comment>"; no fragment means the comment is its own thread.
func threadID(id lore.DocID) string {
	s := string(id)
	if i := strings.LastIndex(s, threadSeparator); i > 0 {
		return s[:i]
	}
	return s
}

// A heading only closes a chunk that has already reached minChunkTokens.
func splitBody(doc lore.Document, body string) []entities.Chunk {
	chunks := make([]entities.Chunk, 0, len(body)/maxChunkBytes+1)
	var prevText string
	emit := func(path []block, text string) {
		if prevText != "" {
			text = overlapOf(prevText) + blockSeparator + text
		}
		prevText = text
		if len(path) > 0 {
			text = strings.Join(headingLines(path), "\n") + blockSeparator + text
		}
		chunks = append(chunks, chunkOf(doc, len(chunks), text, ""))
	}

	var (
		pending     []string
		pendingPath []block
		curBytes    int
		open        []block
	)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		emit(pendingPath, strings.Join(pending, blockSeparator))
		pending, curBytes = pending[:0], 0
	}

	for _, b := range blocksOf(body) {
		size := len(b.text)
		if curBytes > 0 && (b.level > 0 && curBytes >= minChunkBytes || curBytes+len(blockSeparator)+size > maxChunkBytes) {
			flush()
		}
		if b.level > 0 {
			open = closeSections(open, b.level)
		}
		if curBytes == 0 {
			pendingPath = append(pendingPath[:0], open...)
		}
		if b.level > 0 {
			open = append(open, b)
		}
		if size > maxChunkBytes {
			// Keep the tail pending so following blocks can still pack onto it.
			pieces := splitLong(b.text)
			for _, p := range pieces[:len(pieces)-1] {
				emit(pendingPath, p)
			}
			b.text = pieces[len(pieces)-1]
			size = len(b.text)
		}
		if curBytes > 0 {
			curBytes += len(blockSeparator)
		}
		pending, curBytes = append(pending, b.text), curBytes+size
	}
	flush()

	return chunks
}

func closeSections(open []block, level int) []block {
	for len(open) > 0 && open[len(open)-1].level >= level {
		open = open[:len(open)-1]
	}
	return open
}

func headingLines(path []block) []string {
	lines := make([]string, len(path))
	for i, h := range path {
		lines[i] = h.text
	}
	return lines
}

type block struct {
	text  string
	level int
}

// Heading text stays in the chunk: it is the section's context, unlike the title.
func blocksOf(body string) []block {
	var (
		blocks []block
		lines  []string
		fence  string
	)
	flush := func() {
		if len(lines) == 0 {
			return
		}
		blocks = append(blocks, block{text: strings.Join(lines, "\n")})
		lines = lines[:0]
	}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		level := 0
		if fence == "" {
			level = headingLevel(trimmed)
		}
		fence = nextFence(fence, trimmed)
		switch {
		case trimmed == "":
			flush()
		case level > 0:
			flush()
			blocks = append(blocks, block{text: trimmed, level: level})
		default:
			lines = append(lines, line)
		}
	}
	flush()

	return blocks
}

// headingLevel returns the markdown ATX heading level of line, or 0 when it is not a heading.
func headingLevel(line string) int {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level > maxHeadingLevel || level == len(line) || !strings.ContainsRune(wordBreaks, rune(line[level])) {
		return 0
	}
	return level
}

// nextFence returns the fence run still open after line, or "" outside code.
func nextFence(open, line string) string {
	run := fenceRun(line)
	if open == "" {
		if run != "" && run[0] == '`' && strings.ContainsRune(line[len(run):], '`') {
			return ""
		}
		return run
	}
	if run != "" && run[0] == open[0] && len(run) >= len(open) && strings.TrimSpace(line[len(run):]) == "" {
		return ""
	}
	return open
}

func fenceRun(line string) string {
	if line == "" || line[0] != '`' && line[0] != '~' {
		return ""
	}
	n := 1
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < minFenceRun {
		return ""
	}
	return line[:n]
}

// splitLong returns at least one piece for non-empty text; the last may be short.
func splitLong(text string) []string {
	pieces := make([]string, 0, len(text)/maxChunkBytes+1)
	for len(text) > maxChunkBytes {
		cut := cutBefore(text, maxChunkBytes)
		if piece := strings.TrimSpace(text[:cut]); piece != "" {
			pieces = append(pieces, piece)
		}
		text = strings.TrimSpace(text[cut:])
	}
	if text == "" && len(pieces) > 0 {
		return pieces
	}

	return append(pieces, text)
}

// cutBefore returns the last word boundary at or before limit, else a rune boundary.
func cutBefore(s string, limit int) int {
	if i := strings.LastIndexAny(s[:limit], wordBreaks); i > 0 {
		return i
	}
	for limit > 1 && !utf8.RuneStart(s[limit]) {
		limit--
	}

	return limit
}

// Overlap is prepended to a chunk's content, so maxChunkTokens bounds content, not stored text.
func overlapOf(prev string) string {
	if len(prev) <= overlapBytes {
		return prev
	}
	start := len(prev) - overlapBytes
	if i := strings.IndexAny(prev[start:], wordBreaks); i >= 0 {
		start += i + 1
	}
	for start < len(prev) && !utf8.RuneStart(prev[start]) {
		start++
	}

	return strings.TrimSpace(prev[start:])
}

func chunkOf(doc lore.Document, ordinal int, text, thread string) entities.Chunk {
	return entities.Chunk{
		DocID:     doc.ID,
		Ordinal:   ordinal,
		Text:      text,
		Source:    doc.Source,
		RepoRef:   doc.RepoRef,
		DocType:   doc.Type,
		Author:    doc.Author,
		CreatedAt: doc.CreatedAt,
		UpdatedAt: doc.UpdatedAt,
		ThreadID:  thread,
	}
}
