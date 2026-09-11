package config

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const sequenceIndent = "  "

type Field struct {
	Key string
	// Value is a scalar, a []string rendered as a nested sequence, or a
	// []Field rendered as a nested block.
	Value any
}

type Block struct {
	key       string
	lines     []string
	indent    string
	keyLine   int
	found     bool
	flowEmpty bool
	items     []Item
}

type Item struct {
	firstLine int
	lastLine  int
	fields    []itemField
}

type itemField struct {
	key   string
	value string
	line  int
}

func FindBlock(content, key string) (Block, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		return Block{}, internalerror.NewBadRequestError("cannot parse the configuration", err)
	}

	block := Block{key: key, lines: strings.SplitAfter(content, "\n"), indent: sequenceIndent}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return block, nil
	}

	document := root.Content[0]
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

func (b *Block) read(keyLine int, value *yaml.Node) error {
	b.found, b.keyLine = true, keyLine

	switch {
	case value.Kind == yaml.ScalarNode && value.Tag == "!!null" && value.Value == "":
		return nil
	case value.Kind != yaml.SequenceNode:
		return b.refuse()
	case value.Style&yaml.FlowStyle != 0:
		if inline, _ := splitInlineComment(b.keyRest()); inline != "[]" {
			return b.refuse()
		}
		b.flowEmpty = true
		return nil
	}

	b.items, b.indent = b.scanItems()
	for _, node := range value.Content {
		b.readFields(node)
	}
	return nil
}

func (b Block) scanItems() ([]Item, string) {
	items, indent := []Item(nil), sequenceIndent
	for number := b.keyLine + 1; number <= len(b.lines); number++ {
		line := strings.TrimRight(b.lines[number-1], " \t\r\n")
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

func (b *Block) readFields(node *yaml.Node) {
	for at := 0; at+1 < len(node.Content); at += 2 {
		key, value := node.Content[at], node.Content[at+1]
		if value.Kind != yaml.ScalarNode {
			continue
		}
		if item := b.itemAt(value.Line); item != nil {
			item.fields = append(item.fields, itemField{key: key.Value, value: value.Value, line: value.Line})
		}
	}
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
	item, err := encodeItem(b.indent, fields)
	if err != nil {
		return "", err
	}
	if !b.found {
		content := strings.Join(b.lines, "")
		return content + newlineIfMissing(content) + b.key + ":\n" + item, nil
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
			updated.WriteString(newlineIfMissing(line))
			updated.WriteString(item)
		}
	}
	return updated.String(), nil
}

func (b Block) keyRest() string {
	return strings.TrimPrefix(strings.TrimRight(b.lines[b.keyLine-1], " \t\r\n"), b.key+":")
}

func (b Block) reopened() string {
	if _, comment := splitInlineComment(b.keyRest()); comment != "" {
		return b.key + ": " + comment + "\n"
	}
	return b.key + ":\n"
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
	if !found {
		return "", false
	}
	line := strings.TrimRight(b.lines[declared.line-1], "\r\n")
	at := strings.Index(line, field+":")
	if at < 0 {
		return "", false
	}

	scalar, err := encodeScalar(field, value)
	if err != nil {
		return "", false
	}
	_, comment := splitInlineComment(line[at+len(field)+1:])
	replaced := line[:at] + field + ": " + scalar
	if comment != "" {
		replaced += " " + comment
	}
	if strings.HasSuffix(b.lines[declared.line-1], "\n") {
		replaced += "\n"
	}

	var updated strings.Builder
	for number, text := range b.lines {
		if number+1 == declared.line {
			text = replaced
		}
		updated.WriteString(text)
	}
	return updated.String(), true
}

func splitInlineComment(rest string) (value, comment string) {
	if at := strings.Index(rest, "#"); at >= 0 {
		return strings.TrimSpace(rest[:at]), strings.TrimSpace(rest[at:])
	}
	return strings.TrimSpace(rest), ""
}

func newlineIfMissing(text string) string {
	if text == "" || strings.HasSuffix(text, "\n") {
		return ""
	}
	return "\n"
}

func encodeItem(indent string, fields []Field) (string, error) {
	var body strings.Builder
	if err := encodeFields(&body, indent+"  ", fields); err != nil {
		return "", err
	}
	return indent + "- " + strings.TrimPrefix(body.String(), indent+"  "), nil
}

func encodeFields(out *strings.Builder, indent string, fields []Field) error {
	for _, field := range fields {
		switch value := field.Value.(type) {
		case []Field:
			out.WriteString(indent + field.Key + ":\n")
			if err := encodeFields(out, indent+"  ", value); err != nil {
				return err
			}
		case []string:
			out.WriteString(indent + field.Key + ":\n")
			for _, entry := range value {
				scalar, err := encodeScalar(field.Key, entry)
				if err != nil {
					return err
				}
				out.WriteString(indent + "  - " + scalar + "\n")
			}
		default:
			scalar, err := encodeScalar(field.Key, field.Value)
			if err != nil {
				return err
			}
			out.WriteString(indent + field.Key + ": " + scalar + "\n")
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
