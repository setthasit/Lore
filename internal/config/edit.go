package config

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const sequenceIndent = "  "

// yaml.v3 is_break, longest first: LS, PS, CRLF, NEL, LF, CR.
var lineBreaks = [...]string{"\u2028", "\u2029", "\r\n", "\u0085", "\n", "\r"}

type Field struct {
	Key string
	// Value is a scalar, a []string rendered as a nested sequence, or a
	// []Field rendered as a nested block.
	Value any
}

type Block struct {
	key         string
	lines       []string
	indent      string
	rootIndent  string
	newline     string
	keyLine     int
	found       bool
	flowRoot    bool
	flowEmpty   bool
	flowComment string
	flowHead    string
	flowTail    string
	items       []Item
}

type Item struct {
	firstLine int
	lastLine  int
	fields    []itemField
}

type itemField struct {
	key        string
	value      string
	line       int
	head       string
	tail       string
	spliceable bool
}

func FindBlock(content, key string) (Block, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		return Block{}, internalerror.NewBadRequestError("cannot parse the configuration", err)
	}

	block := Block{key: key, lines: splitLines(content), indent: sequenceIndent}
	block.newline = documentNewline(block.lines)
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return block, nil
	}

	document := root.Content[0]
	block.flowRoot = document.Style&yaml.FlowStyle != 0
	block.rootIndent = indentOf(block.line(document.Line))
	block.indent = block.rootIndent + sequenceIndent
	for at := 0; at+1 < len(document.Content); at += 2 {
		if document.Content[at].Value != key {
			continue
		}
		if err := block.read(document.Content[at].Line, document.Content[at+1]); err != nil {
			return Block{}, err
		}
		break
	}
	return block, nil
}

func (b Block) line(number int) string {
	if number < 1 || number > len(b.lines) {
		return ""
	}
	return b.lines[number-1]
}

func splitLines(content string) []string {
	breaks := 0
	for _, breaker := range lineBreaks {
		breaks += strings.Count(content, breaker)
	}

	lines, start := make([]string, 0, breaks+1), 0
	for at := 0; at < len(content); {
		width := lineBreakWidth(content[at:])
		if width == 0 {
			at++
			continue
		}
		at += width
		lines = append(lines, content[start:at])
		start = at
	}
	return append(lines, content[start:])
}

func lineBreakWidth(text string) int {
	for _, breaker := range lineBreaks {
		if strings.HasPrefix(text, breaker) {
			return len(breaker)
		}
	}
	return 0
}

func indentOf(line string) string {
	line = strings.TrimPrefix(line, "\ufeff")
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func documentNewline(lines []string) string {
	for _, line := range lines {
		if terminator := terminatorOf(line); terminator != "" {
			return terminator
		}
	}
	return "\n"
}

func terminatorOf(line string) string {
	for _, breaker := range lineBreaks {
		if strings.HasSuffix(line, breaker) {
			return breaker
		}
	}
	return ""
}

func (b *Block) read(keyLine int, value *yaml.Node) error {
	b.found, b.keyLine = true, keyLine

	switch {
	case b.flowRoot:
		return b.refuseDocument()
	case value.Kind == yaml.ScalarNode && value.Tag == "!!null" && value.Value == "":
		return nil
	case value.Kind != yaml.SequenceNode:
		return b.refuse()
	case value.Style&yaml.FlowStyle != 0:
		head, tail, reopenable := b.reopenableEmptyFlow(value)
		if !reopenable {
			return b.refuse()
		}
		b.flowEmpty, b.flowComment = true, value.LineComment
		b.flowHead, b.flowTail = head, tail
		b.indent = indentOf(head) + sequenceIndent
		return nil
	}

	b.items, b.indent = b.scanItems()
	for _, node := range value.Content {
		b.readFields(node)
	}
	return nil
}

func (b Block) reopenableEmptyFlow(value *yaml.Node) (head, tail string, reopenable bool) {
	if len(value.Content) != 0 || value.Anchor != "" ||
		value.Style&yaml.TaggedStyle != 0 || !b.flowClosesOnKeyLine() {
		return "", "", false
	}

	raw := b.line(b.keyLine)
	body, at, aligned := valueSplit(raw, value.Column)
	if !aligned {
		return "", "", false
	}

	gap := body[at:]
	if !strings.HasSuffix(gap, value.LineComment) {
		return "", "", false
	}
	if !onlyEmptyFlow(strings.TrimRight(gap[:len(gap)-len(value.LineComment)], " \t")) {
		return "", "", false
	}
	return strings.TrimRight(body[:at], " \t"), raw[len(body):], true
}

func onlyEmptyFlow(closed string) bool {
	return strings.HasPrefix(closed, "[") && strings.HasSuffix(closed, "]") &&
		strings.Trim(closed, "[] \t") == ""
}

func valueSplit(raw string, column int) (body string, at int, aligned bool) {
	body = strings.TrimSuffix(raw, terminatorOf(raw))
	at = column - 1
	if at > len(body) || utf8.RuneCountInString(body[:at]) != at {
		return "", 0, false
	}
	return body, at, true
}

func (b Block) flowClosesOnKeyLine() bool {
	var parsed yaml.Node
	if err := yaml.Unmarshal([]byte(b.line(b.keyLine)), &parsed); err != nil {
		return false
	}
	if len(parsed.Content) != 1 || parsed.Content[0].Kind != yaml.MappingNode ||
		len(parsed.Content[0].Content) != 2 {
		return false
	}

	closed := parsed.Content[0].Content[1]
	return closed.Kind == yaml.SequenceNode && len(closed.Content) == 0
}

func (b Block) scanItems() ([]Item, string) {
	items, indent := []Item(nil), sequenceIndent
	for number := b.keyLine + 1; number <= len(b.lines); number++ {
		raw := b.lines[number-1]
		line := strings.TrimRight(strings.TrimSuffix(raw, terminatorOf(raw)), " \t")
		body := strings.TrimLeft(line, " \t")
		if body == "" || strings.HasPrefix(body, "#") {
			continue
		}

		lineIndent := line[:len(line)-len(body)]
		if body == "-" || strings.HasPrefix(body, "- ") {
			if len(items) == 0 || lineIndent == indent {
				indent = lineIndent
				items = append(items, Item{firstLine: number, lastLine: number})
				continue
			}
		}
		if len(items) == 0 || len(lineIndent) <= len(indent) {
			break
		}
		items[len(items)-1].lastLine = number
	}
	return items, indent
}

func (b Block) refuse() error {
	return internalerror.NewPreconditionError(b.key+": in the configuration carries an inline value no"+
		" edit can splice — rewrite it as a block sequence of items", nil)
}

func (b Block) refuseDocument() error {
	return internalerror.NewPreconditionError("the configuration is written as one inline mapping no edit"+
		" can splice — rewrite it as a block mapping with one key per line", nil)
}

func (b *Block) readFields(node *yaml.Node) {
	for at := 0; at+1 < len(node.Content); at += 2 {
		key, value := node.Content[at], node.Content[at+1]
		if value.Kind != yaml.ScalarNode {
			continue
		}
		item := b.itemAt(value.Line)
		if item == nil {
			continue
		}

		head, tail, spliceable := spliceAround(b.line(value.Line), key, value)
		item.fields = append(item.fields, itemField{
			key:        key.Value,
			value:      value.Value,
			line:       value.Line,
			head:       head,
			tail:       tail,
			spliceable: spliceable,
		})
	}
}

func spliceAround(raw string, key, value *yaml.Node) (head, tail string, spliceable bool) {
	if key.Line != value.Line {
		return "", "", false
	}

	body, at, aligned := valueSplit(raw, value.Column)
	if !aligned {
		return "", "", false
	}
	encoded, err := bareScalar(value)
	if err != nil || encoded == "" || !strings.HasPrefix(body[at:], encoded) {
		return "", "", false
	}

	gap := body[at+len(encoded):]
	if !strings.HasSuffix(gap, value.LineComment) {
		return "", "", false
	}
	spacing := gap[:len(gap)-len(value.LineComment)]
	if strings.TrimLeft(spacing, " \t") != "" {
		return "", "", false
	}
	if spacing == "" && value.LineComment != "" {
		spacing = " "
	}
	return body[:at], spacing + value.LineComment + raw[len(body):], true
}

func bareScalar(value *yaml.Node) (string, error) {
	bare := yaml.Node{Kind: yaml.ScalarNode, Style: value.Style &^ yaml.TaggedStyle, Value: value.Value}
	encoded, err := yaml.Marshal(&bare)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(encoded), "\n"), nil
}

func (b *Block) itemAt(line int) *Item {
	for at := range b.items {
		if line >= b.items[at].firstLine && line <= b.items[at].lastLine {
			return &b.items[at]
		}
	}
	return nil
}

func (b Block) Find(name string) (Item, bool) {
	for _, item := range b.items {
		if field, declared := item.field("name"); declared && field.value == name {
			return item, true
		}
	}
	return Item{}, false
}

func (i Item) field(key string) (itemField, bool) {
	for _, field := range i.fields {
		if field.key == key {
			return field, true
		}
	}
	return itemField{}, false
}

func (b Block) AppendItem(fields []Field) (string, error) {
	if b.flowRoot {
		return "", b.refuseDocument()
	}
	item, err := encodeItem(b.indent, b.newline, fields)
	if err != nil {
		return "", err
	}
	if !b.found {
		content := strings.Join(b.lines, "")
		return content + b.newlineIfMissing(content) +
			b.rootIndent + b.key + ":" + b.newline + item, nil
	}

	after := b.keyLine
	if last := len(b.items) - 1; last >= 0 {
		after = b.items[last].lastLine
	}

	var updated strings.Builder
	for at, line := range b.lines {
		if at+1 == b.keyLine && b.flowEmpty {
			line = b.reopened()
		}
		updated.WriteString(line)
		if at+1 == after {
			updated.WriteString(b.newlineIfMissing(line))
			updated.WriteString(item)
		}
	}
	return updated.String(), nil
}

func (b Block) reopened() string {
	if b.flowComment != "" {
		return b.flowHead + " " + b.flowComment + b.flowTail
	}
	return b.flowHead + b.flowTail
}

func (b Block) Remove(item Item) string {
	dropKey := len(b.items) == 1

	var kept strings.Builder
	for at, line := range b.lines {
		number := at + 1
		if number >= item.firstLine && number <= item.lastLine {
			continue
		}
		if dropKey && number == b.keyLine {
			continue
		}
		kept.WriteString(line)
	}
	return kept.String()
}

func (b Block) SetField(item Item, field, value string) (string, bool) {
	declared, found := item.field(field)
	if !found || !declared.spliceable {
		return "", false
	}
	scalar, err := encodeScalar(field, value)
	if err != nil {
		return "", false
	}

	replaced := declared.head + scalar + declared.tail

	var updated strings.Builder
	for number, text := range b.lines {
		if number+1 == declared.line {
			text = replaced
		}
		updated.WriteString(text)
	}
	return updated.String(), true
}

func (b Block) newlineIfMissing(text string) string {
	if text == "" || terminatorOf(text) != "" {
		return ""
	}
	return b.newline
}

func encodeItem(indent, newline string, fields []Field) (string, error) {
	var body strings.Builder
	if err := encodeFields(&body, indent+"  ", newline, fields); err != nil {
		return "", err
	}
	return indent + "- " + strings.TrimPrefix(body.String(), indent+"  "), nil
}

func encodeFields(out *strings.Builder, indent, newline string, fields []Field) error {
	for _, field := range fields {
		switch value := field.Value.(type) {
		case []Field:
			out.WriteString(indent + field.Key + ":" + newline)
			if err := encodeFields(out, indent+"  ", newline, value); err != nil {
				return err
			}
		case []string:
			out.WriteString(indent + field.Key + ":" + newline)
			for _, entry := range value {
				scalar, err := encodeScalar(field.Key, entry)
				if err != nil {
					return err
				}
				out.WriteString(indent + "  - " + scalar + newline)
			}
		default:
			scalar, err := encodeScalar(field.Key, field.Value)
			if err != nil {
				return err
			}
			out.WriteString(indent + field.Key + ": " + scalar + newline)
		}
	}
	return nil
}

func encodeScalar(key string, value any) (string, error) {
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return "", internalerror.NewInternalError("cannot encode the value for "+key, err)
	}
	scalar := strings.TrimSuffix(string(encoded), "\n")
	if strings.Contains(scalar, "\n") {
		// A value holding a newline marshals as a block scalar, which a one-line splice cannot carry.
		if text, isText := value.(string); isText {
			return strconv.Quote(text), nil
		}
		return "", internalerror.NewInternalError("cannot encode the value for "+key+" on one line", nil)
	}
	return scalar, nil
}
