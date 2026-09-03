package services

import (
	"strings"
	"unicode/utf8"

	"github.com/setthasit/Lore/internal/entities"
)

type Chunker interface {
	// Chunk returns doc's chunks in body order, Ordinal 0-based; a blank body yields none.
	Chunk(doc entities.Document) []entities.Chunk
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

// blockSeparator joins blocks, and separates carried overlap from a chunk's own content.
const blockSeparator = "\n\n"

const threadSeparator = "#"

const wordBreaks = " \t\n"

type chunker struct{}

var _ Chunker = chunker{}

func NewChunker() Chunker { return chunker{} }

func (chunker) Chunk(doc entities.Document) []entities.Chunk {
	body := strings.TrimSpace(doc.Body)
	if body == "" {
		return nil
	}

	switch doc.Type {
	case entities.DocTypeCommit:
		return []entities.Chunk{chunkOf(doc, 0, body, "")}
	case entities.DocTypeReviewComment, entities.DocTypeIssueComment, entities.DocTypeTicketComment:
		return []entities.Chunk{chunkOf(doc, 0, body, threadID(doc.ID))}
	default:
		return splitBody(doc, body)
	}
}

// A comment DocID is "<thread>#<comment>"; no fragment means the comment is its own thread.
func threadID(id entities.DocID) string {
	s := string(id)
	if i := strings.LastIndex(s, threadSeparator); i > 0 {
		return s[:i]
	}
	return s
}

// A heading only closes a chunk that has already reached minChunkTokens.
func splitBody(doc entities.Document, body string) []entities.Chunk {
	chunks := make([]entities.Chunk, 0, len(body)/maxChunkBytes+1)
	emit := func(text string) {
		if len(chunks) > 0 {
			text = overlapOf(chunks[len(chunks)-1].Text) + blockSeparator + text
		}
		chunks = append(chunks, chunkOf(doc, len(chunks), text, ""))
	}

	var (
		pending  []string
		curBytes int
	)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		emit(strings.Join(pending, blockSeparator))
		pending, curBytes = pending[:0], 0
	}

	for _, b := range blocksOf(body) {
		size := len(b.text)
		if curBytes > 0 && (b.heading && curBytes >= minChunkBytes || curBytes+len(blockSeparator)+size > maxChunkBytes) {
			flush()
		}
		if size > maxChunkBytes {
			// Keep the tail pending so following blocks can still pack onto it.
			pieces := splitLong(b.text)
			for _, p := range pieces[:len(pieces)-1] {
				emit(p)
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

type block struct {
	text    string
	heading bool
}

// Heading text stays in the chunk: it is the section's context, unlike the title.
func blocksOf(body string) []block {
	var (
		blocks []block
		lines  []string
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
		switch {
		case trimmed == "":
			flush()
		case isHeading(trimmed):
			flush()
			blocks = append(blocks, block{text: trimmed, heading: true})
		default:
			lines = append(lines, line)
		}
	}
	flush()

	return blocks
}

func isHeading(line string) bool {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	return level > 0 && level <= 6 && level < len(line) && strings.ContainsRune(wordBreaks, rune(line[level]))
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

func chunkOf(doc entities.Document, ordinal int, text, thread string) entities.Chunk {
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
