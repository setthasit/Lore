package services

import (
	"strings"
	"unicode/utf8"

	"lore/internal/entities"
)

// Chunker slices a normalized document body into embedding-sized chunks. The
// strategy is type-aware (03-data-model.md, "Chunking"):
//
//   - commit: the whole message is one chunk. The subject is already weighted
//     into the document's Title, so Title is never duplicated into chunk text.
//   - review_comment / issue_comment / ticket_comment: one comment is one chunk,
//     tagged with its thread so retrieval can rehydrate the conversation.
//   - pr / issue / ticket / page, and every type the table does not name:
//     heading and paragraph split, ~300-500 estimated tokens, small overlap.
//
// Chunking is derivation, not I/O: it cannot fail, so there is no error return
// and nothing to classify into an internalerror kind. Chunks come back with a
// nil Embedding; the sync service fills vectors after chunking.
//
// The interface exists for the sync service's tests to fake; the single
// implementation is stateless.
type Chunker interface {
	// Chunk returns the chunks of doc in body order, Ordinal 0-based, each
	// carrying doc's filterable metadata. An empty (or whitespace-only) body
	// yields no chunks: empty chunk rows would only pollute the index.
	Chunk(doc entities.Document) []entities.Chunk
}

// Chunk sizes are measured in estimated tokens, and the estimate is
// len(text)/4 bytes per token — the standard rule of thumb for English prose
// and code in BPE tokenizers. A real tokenizer would cost a dependency and a
// model-specific vocabulary to buy accuracy that chunk boundaries do not need.
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

// blockSeparator joins paragraphs and headings back into chunk text, and also
// separates the carried overlap from the chunk's own content.
const blockSeparator = "\n\n"

// threadSeparator splits a comment's DocID into thread and comment parts.
const threadSeparator = "#"

// wordBreaks are the byte-level cut points for splitting text that has no
// paragraph structure to split on.
const wordBreaks = " \t\n"

type chunker struct{}

var _ Chunker = chunker{}

// NewChunker returns the type-aware chunker.
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

// threadID derives a comment's thread identity from its document identity.
// Document has no thread field and does not need one: a comment is addressed on
// the web as a fragment of the page that holds the thread
// ("…/pull/42#discussion_r7", "…/issues/42#issuecomment-9", "PROJ-1#10042"), so
// connectors mint comment DocIDs as "<thread>#<comment>". Dropping the fragment
// leaves the thread-scoped prefix: identical for every comment in the thread,
// source-qualified and therefore globally unique, and derived from data that
// already exists. A DocID with no fragment is a comment that stands alone, so it
// is its own thread.
func threadID(id entities.DocID) string {
	s := string(id)
	if i := strings.LastIndex(s, threadSeparator); i > 0 {
		return s[:i]
	}
	return s
}

// splitBody packs the body's blocks into chunks. Markdown headings are preferred
// break points rather than mandatory ones: breaking at every heading would emit
// a chunk per one-line section and miss the ~300-token target, so a heading only
// closes a chunk that has already reached minChunkTokens. Otherwise a chunk
// closes when the next block would push it past maxChunkTokens.
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
			// A single block too big to be a chunk: cut it at word boundaries and
			// keep the tail pending so following blocks can still pack onto it.
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

// block is one structural unit of a body: a paragraph, or a markdown heading
// line, which marks a candidate chunk boundary.
type block struct {
	text    string
	heading bool
}

// blocksOf splits a body into blank-line separated paragraphs, with ATX
// markdown headings standing alone as boundary blocks. Heading text stays in the
// chunk: it is the section's context, unlike the document title.
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

// isHeading reports whether a trimmed line is an ATX markdown heading.
func isHeading(line string) bool {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	return level > 0 && level <= 6 && level < len(line) && strings.ContainsRune(wordBreaks, rune(line[level]))
}

// splitLong cuts text with no usable paragraph structure into pieces of at most
// maxChunkTokens, breaking at word boundaries. It returns at least one piece for
// non-empty text; the last one is whatever remains and may be short.
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

// cutBefore returns the last word boundary at or before limit, falling back to a
// rune boundary when a single word is longer than a whole chunk.
func cutBefore(s string, limit int) int {
	if i := strings.LastIndexAny(s[:limit], wordBreaks); i > 0 {
		return i
	}
	for limit > 1 && !utf8.RuneStart(s[limit]) {
		limit--
	}

	return limit
}

// overlapOf returns the trailing ~overlapTokens of the previous chunk, cut at
// the first word boundary inside that window. Adjacent chunks therefore share a
// sentence or two, so a match that straddles a boundary still retrieves readable
// context. Overlap is prepended to a chunk's own content, which is why
// maxChunkTokens bounds the content rather than the stored text.
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

// chunkOf copies the parent document's filterable metadata onto one chunk.
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
