package notion

import (
	"strconv"
	"strings"
	"testing"
)

func textBlock(kind, text string) block {
	return block{Type: kind, Payload: blockPayload{RichText: []richText{{PlainText: text}}}}
}

func nest(parent block, children ...block) block {
	parent.HasChildren = true
	parent.Children = children
	return parent
}

func TestFlattenRendersEveryBlockKind(t *testing.T) {
	tests := []struct {
		name   string
		blocks []block
		want   string
	}{
		{
			name:   "headings",
			blocks: []block{textBlock(blockHeading1, "One"), textBlock(blockHeading2, "Two"), textBlock(blockHeading3, "Three")},
			want:   "# One\n## Two\n### Three",
		},
		{
			name:   "paragraph, callout and toggle are plain lines",
			blocks: []block{textBlock("paragraph", "Plain"), textBlock("callout", "Heads up"), textBlock("toggle", "Details")},
			want:   "Plain\nHeads up\nDetails",
		},
		{
			name:   "quote",
			blocks: []block{textBlock(blockQuote, "Ship it")},
			want:   "> Ship it",
		},
		{
			name:   "list items",
			blocks: []block{textBlock(blockBulleted, "First"), textBlock(blockNumbered, "Second")},
			want:   "- First\n- Second",
		},
		{
			name: "to_do carries its checkbox state",
			blocks: []block{
				{Type: blockToDo, Payload: blockPayload{RichText: []richText{{PlainText: "Done"}}, Checked: true}},
				{Type: blockToDo, Payload: blockPayload{RichText: []richText{{PlainText: "Open"}}}},
			},
			want: "- [x] Done\n- [ ] Open",
		},
		{
			name: "code fence carries the language",
			blocks: []block{
				{Type: blockCode, Payload: blockPayload{RichText: []richText{{PlainText: "x := 1"}}, Language: "go"}},
			},
			want: "```go\nx := 1\n```",
		},
		{
			name:   "divider",
			blocks: []block{{Type: blockDivider}},
			want:   "---",
		},
		{
			name: "table row joins its cells",
			blocks: []block{{Type: blockTableRow, Payload: blockPayload{Cells: [][]richText{
				{{PlainText: "Region"}},
				{{PlainText: "Owner"}},
				{},
			}}}},
			want: "Region | Owner | ",
		},
		{
			name:   "empty table row emits nothing",
			blocks: []block{{Type: blockTableRow}},
			want:   "",
		},
		{
			name: "child page is named but never inlined",
			blocks: []block{nest(
				block{Type: blockChildPage, Payload: blockPayload{Title: "Deep Runbook"}},
				textBlock("paragraph", "body of the child page"),
			)},
			want: "Deep Runbook",
		},
		{
			name:   "unknown block contributes its rich text",
			blocks: []block{textBlock("synced_block", "Shared note")},
			want:   "Shared note",
		},
		{
			name:   "unknown block without rich text degrades to nothing",
			blocks: []block{{Type: "audio", Payload: blockPayload{Language: "ignored"}}},
			want:   "",
		},
		{
			name:   "empty rich text emits no line",
			blocks: []block{textBlock("paragraph", ""), textBlock(blockHeading1, "")},
			want:   "",
		},
		{
			name: "nested list items indent",
			blocks: []block{nest(
				textBlock(blockBulleted, "Rotate keys"),
				nest(textBlock(blockBulleted, "Notify owners"), textBlock(blockNumbered, "File the ticket")),
			)},
			want: "- Rotate keys\n  - Notify owners\n    - File the ticket",
		},
		{
			name: "link keeps its target",
			blocks: []block{{Type: "paragraph", Payload: blockPayload{RichText: []richText{
				{PlainText: "see "},
				{PlainText: "the ticket", Href: "https://acme.atlassian.net/browse/PROJ-9"},
			}}}},
			want: "see [the ticket](https://acme.atlassian.net/browse/PROJ-9)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flatten(tt.blocks); got != tt.want {
				t.Errorf("flatten\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestFlattenStopsAtTheDepthCap(t *testing.T) {
	deepest := maxBlockDepth + 2
	node := textBlock("paragraph", "level "+strconv.Itoa(deepest))
	for depth := deepest - 1; depth >= 0; depth-- {
		node = nest(textBlock("paragraph", "level "+strconv.Itoa(depth)), node)
	}

	body := flatten([]block{node})
	if last := "level " + strconv.Itoa(maxBlockDepth); !strings.Contains(body, last) {
		t.Errorf("%q missing from\n%s", last, body)
	}
	if beyond := "level " + strconv.Itoa(maxBlockDepth+1); strings.Contains(body, beyond) {
		t.Errorf("%q survived the depth cap in\n%s", beyond, body)
	}
}

func TestRichTextRendering(t *testing.T) {
	tests := []struct {
		name string
		item richText
		want string
	}{
		{name: "plain", item: richText{PlainText: "hello"}, want: "hello"},
		{
			name: "display text hiding the url",
			item: richText{PlainText: "the ticket", Href: "https://acme.atlassian.net/browse/PROJ-1"},
			want: "[the ticket](https://acme.atlassian.net/browse/PROJ-1)",
		},
		{
			name: "text already equal to the url",
			item: richText{PlainText: "https://acme.test/a", Href: "https://acme.test/a"},
			want: "https://acme.test/a",
		},
		{
			name: "mention without display text",
			item: richText{Href: "https://acme.test/a"},
			want: "https://acme.test/a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.item.text(); got != tt.want {
				t.Errorf("text() = %q, want %q", got, tt.want)
			}
		})
	}
}
