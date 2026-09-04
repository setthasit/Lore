package notion

import "strings"

const (
	// maxBlockDepth bounds both the child fetches and the walk over a block tree.
	maxBlockDepth = 8

	indentUnit = "  "

	blockHeading1  = "heading_1"
	blockHeading2  = "heading_2"
	blockHeading3  = "heading_3"
	blockQuote     = "quote"
	blockBulleted  = "bulleted_list_item"
	blockNumbered  = "numbered_list_item"
	blockToDo      = "to_do"
	blockCode      = "code"
	blockDivider   = "divider"
	blockTableRow  = "table_row"
	blockChildPage = "child_page"
)

func flatten(blocks []block) string {
	var out strings.Builder
	writeBlocks(&out, blocks, 0)
	return strings.TrimRight(out.String(), "\n")
}

func writeBlocks(out *strings.Builder, blocks []block, depth int) {
	if depth > maxBlockDepth {
		return
	}
	for i := range blocks {
		b := &blocks[i]
		if line, ok := render(b, depth); ok {
			out.WriteString(line)
			out.WriteByte('\n')
		}
		// A child page is ingested as its own document; inlining its blocks would duplicate the body.
		if b.Type != blockChildPage {
			writeBlocks(out, b.Children, depth+1)
		}
	}
}

func render(b *block, depth int) (string, bool) {
	switch b.Type {
	case blockDivider:
		return "---", true
	case blockCode:
		return "```" + b.Payload.Language + "\n" + plainText(b.Payload.RichText) + "\n```", true
	case blockTableRow:
		return joinCells(b.Payload.Cells), len(b.Payload.Cells) > 0
	case blockChildPage:
		return b.Payload.Title, b.Payload.Title != ""
	}
	text := plainText(b.Payload.RichText)
	return linePrefix(b, depth) + text, text != ""
}

func linePrefix(b *block, depth int) string {
	switch b.Type {
	case blockHeading1:
		return "# "
	case blockHeading2:
		return "## "
	case blockHeading3:
		return "### "
	case blockQuote:
		return "> "
	case blockBulleted, blockNumbered:
		return indent(depth) + "- "
	case blockToDo:
		if b.Payload.Checked {
			return indent(depth) + "- [x] "
		}
		return indent(depth) + "- [ ] "
	}
	return ""
}

func indent(depth int) string { return strings.Repeat(indentUnit, depth) }

func joinCells(cells [][]richText) string {
	texts := make([]string, 0, len(cells))
	for _, cell := range cells {
		texts = append(texts, plainText(cell))
	}
	return strings.Join(texts, " | ")
}

func plainText(items []richText) string {
	var out strings.Builder
	for _, item := range items {
		out.WriteString(item.text())
	}
	return strings.TrimSpace(out.String())
}

// A link whose display text hides its URL still has to yield a URL reference downstream.
func (r richText) text() string {
	switch {
	case r.Href == "":
		return r.PlainText
	case r.PlainText == "" || r.PlainText == r.Href:
		return r.Href
	}
	return "[" + r.PlainText + "](" + r.Href + ")"
}
