package jira

import (
	"strconv"
	"strings"
	"time"
)

const maxADFDepth = 24

type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text"`
	Attrs   adfAttrs  `json:"attrs"`
	Marks   []adfMark `json:"marks"`
	Content []adfNode `json:"content"`
}

type adfAttrs struct {
	Level     int    `json:"level"`
	Language  string `json:"language"`
	URL       string `json:"url"`
	Text      string `json:"text"`
	ShortName string `json:"shortName"`
	Timestamp string `json:"timestamp"`
}

type adfMark struct {
	Type  string `json:"type"`
	Attrs struct {
		Href string `json:"href"`
	} `json:"attrs"`
}

func flatten(doc *adfNode) string {
	if doc == nil {
		return ""
	}
	var r renderer
	r.block(doc, 0, 0)
	return strings.TrimSpace(strings.Join(r.lines, "\n"))
}

type renderer struct {
	lines []string
}

func (r *renderer) block(n *adfNode, depth, indent int) {
	if depth > maxADFDepth {
		return
	}
	switch n.Type {
	case "doc", "panel", "table":
		r.children(n, depth, indent)
	case "paragraph":
		r.emit(indent, childText(n, depth))
	case "heading":
		if text := childText(n, depth); text != "" {
			r.emit(indent, headingPrefix(n.Attrs.Level)+" "+text)
		}
	case "bulletList", "orderedList":
		r.list(n, depth, indent)
	case "blockquote":
		r.quote(n, depth, indent)
	case "codeBlock":
		r.code(n, depth, indent)
	case "tableRow":
		r.emit(indent, row(n, depth))
	case "rule":
		r.emit(indent, "---")
	default:
		r.emit(indent, inline(n, depth))
	}
}

func (r *renderer) children(n *adfNode, depth, indent int) {
	for i := range n.Content {
		r.block(&n.Content[i], depth+1, indent)
	}
}

func (r *renderer) list(n *adfNode, depth, indent int) {
	for i := range n.Content {
		item := &n.Content[i]
		if item.Type != "listItem" {
			r.block(item, depth+1, indent)
			continue
		}
		var sub renderer
		sub.children(item, depth+1, 0)
		for j, line := range sub.lines {
			if j == 0 {
				r.emit(indent, "- "+line)
				continue
			}
			r.emit(indent+1, line)
		}
	}
}

func (r *renderer) quote(n *adfNode, depth, indent int) {
	var sub renderer
	sub.children(n, depth, 0)
	for _, line := range sub.lines {
		r.emit(indent, "> "+line)
	}
}

func (r *renderer) code(n *adfNode, depth, indent int) {
	r.emit(indent, "```"+n.Attrs.Language)
	for _, line := range strings.Split(childText(n, depth), "\n") {
		r.emit(indent, line)
	}
	r.emit(indent, "```")
}

func (r *renderer) emit(indent int, text string) {
	if text == "" {
		return
	}
	r.lines = append(r.lines, strings.Repeat("  ", indent)+text)
}

func row(n *adfNode, depth int) string {
	cells := make([]string, 0, len(n.Content))
	for i := range n.Content {
		var sub renderer
		sub.children(&n.Content[i], depth+1, 0)
		cells = append(cells, strings.Join(sub.lines, " "))
	}
	return strings.Join(cells, " | ")
}

func inline(n *adfNode, depth int) string {
	if depth > maxADFDepth {
		return ""
	}
	switch n.Type {
	case "text":
		if href := linkHref(n.Marks); href != "" {
			return "[" + n.Text + "](" + href + ")"
		}
		return n.Text
	case "hardBreak":
		return "\n"
	case "inlineCard":
		return n.Attrs.URL
	case "mention", "status":
		return n.Attrs.Text
	case "emoji":
		return n.Attrs.ShortName
	case "date":
		return epochDate(n.Attrs.Timestamp)
	}
	return childText(n, depth)
}

func childText(n *adfNode, depth int) string {
	var b strings.Builder
	for i := range n.Content {
		b.WriteString(inline(&n.Content[i], depth+1))
	}
	return b.String()
}

func linkHref(marks []adfMark) string {
	for _, m := range marks {
		if m.Type == "link" {
			return m.Attrs.Href
		}
	}
	return ""
}

func headingPrefix(level int) string {
	switch {
	case level < 1:
		level = 1
	case level > 6:
		level = 6
	}
	return strings.Repeat("#", level)
}

func epochDate(raw string) string {
	millis, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return raw
	}
	return time.UnixMilli(millis).UTC().Format(time.DateOnly)
}
