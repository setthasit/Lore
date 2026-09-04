package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/sdk"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// sourcesKey is the top-level key a new instance is appended under, and the only
// thing this file knows about the shape of a configuration.
const sourcesKey = "sources:"

// sequenceIndent is where an item goes in a block this command creates itself.
// An existing block's own indentation is read from it instead.
const sequenceIndent = "  "

func newSourceCommand(configPath *string, reg *registry.Registry) *cobra.Command {
	source := &cobra.Command{
		Use:   "source",
		Short: "Manage the sources lore.yaml ingests",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	source.AddCommand(newSourceAddCommand(configPath, reg))
	return source
}

func newSourceAddCommand(configPath *string, reg *registry.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "add <" + strings.Join(sourceArgument(reg), "|") + ">",
		Short: "Append a source instance to lore.yaml, asking for the fields its plugin declares",
		Long: "Asks for exactly what the plugin's manifest declares and appends the answers\n" +
			"as an item under sources: in lore.yaml, leaving every existing line\n" +
			"untouched. It asks for the NAME of the environment variable holding each\n" +
			"credential, never the credential.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSourceAdd(cmd, args, *configPath, reg)
		},
	}
}

// A build with no source plugin has no name to put in the usage line, and the
// invocation is refused by sourceToAdd with an error that explains that.
func sourceArgument(reg *registry.Registry) []string {
	if names := reg.Names(lore.KindSource); len(names) > 0 {
		return names
	}
	return []string{"plugin"}
}

func runSourceAdd(cmd *cobra.Command, args []string, configPath string, reg *registry.Registry) error {
	manifest, err := sourceToAdd(args, reg)
	if err != nil {
		return err
	}

	original, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return internalerror.NewNotFoundError("no configuration at "+configPath+" — run `lore init` to create one", err)
		}
		return internalerror.NewInternalError("cannot read "+configPath, err)
	}
	current, err := config.Decode(bytes.NewReader(original))
	if err != nil {
		return internalerror.NewBadRequestError("cannot parse "+configPath, err)
	}

	draft, err := promptSource(&prompter{
		in:  bufio.NewReader(cmd.InOrStdin()),
		out: cmd.OutOrStdout(),
	}, manifest, current)
	if err != nil {
		return err
	}

	updated, err := insertSource(string(original), draft)
	if err != nil {
		return err
	}
	// The spliced result is decoded before it is written, so a file that would
	// no longer load is refused while the one on disk is still the old one.
	if _, err := config.Decode(strings.NewReader(updated)); err != nil {
		return internalerror.NewInternalError("the "+draft.ident()+" instance does not fit "+
			configPath+", which is unchanged", err)
	}
	if err := replaceFile(configPath, updated); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	printfln(out, "added sources[%s] to %s", draft.ident(), configPath)
	if len(draft.variables) > 0 {
		printfln(out, "next: export %s, then run `lore sync`", strings.Join(draft.variables, " and "))
	} else {
		printfln(out, "next: run `lore sync`")
	}
	return nil
}

// sourceToAdd resolves the argument against the registry, so this command
// accepts whatever plugins the binary was built with and nothing else.
func sourceToAdd(args []string, reg *registry.Registry) (lore.Manifest, error) {
	names := reg.Names(lore.KindSource)
	registered := "the source plugins this build registers are " + strings.Join(names, ", ")
	if len(names) == 0 {
		registered = "this build registers no source plugin at all"
	}

	if len(args) == 0 {
		return lore.Manifest{}, internalerror.NewBadRequestError("name the source plugin to add: "+registered, nil)
	}
	manifest, known := reg.Manifest(args[0])
	if !known || manifest.Kind != lore.KindSource {
		return lore.Manifest{}, internalerror.NewBadRequestError("unknown source plugin "+args[0]+
			" — "+registered+"; run `lore plugin list` to see them all", nil)
	}
	return manifest, nil
}

// sourceDraft is what one round of prompting produced: the identity the instance
// will carry, its `with:` keys in manifest order, and the variables the operator
// now has to export.
type sourceDraft struct {
	id        string
	use       string
	entries   []withEntry
	variables []string
}

func (d sourceDraft) ident() string {
	if d.id != "" {
		return d.id
	}
	return d.use
}

// withEntry is one rendered key of a `with:` block. The block is built as an
// ordered list rather than a map because a map would sort the keys and lose the
// order the manifest declared them in.
type withEntry struct {
	key   string
	value any // string, int, bool or []string
}

// promptSource asks for everything the manifest declares and nothing else: a
// plugin that adds a field gets a prompt for it without this file changing.
func promptSource(p *prompter, m lore.Manifest, current *config.Config) (sourceDraft, error) {
	draft := sourceDraft{use: m.Name}

	id, err := promptInstanceID(p, m.Name, current.Sources)
	if err != nil {
		return draft, err
	}
	draft.id = id

	field := "sources[" + draft.ident() + "].with."
	for _, secret := range m.Secrets {
		name, err := p.envName(field+secret.ConfigField, secretHolds(m, secret), secret.DefaultEnv)
		if err != nil {
			return draft, err
		}
		draft.entries = append(draft.entries, withEntry{key: secret.ConfigField, value: name})
		draft.variables = append(draft.variables, name)
	}
	for _, declared := range m.Fields {
		value, set, err := promptField(p, field+declared.Name, declared)
		if err != nil {
			return draft, err
		}
		if set {
			draft.entries = append(draft.entries, withEntry{key: declared.Name, value: value})
		}
	}
	return draft, nil
}

// promptInstanceID asks for an id only when the plugin's name is already taken,
// which is exactly when a second instance of one plugin needs one: config
// rejects two instances sharing an identity, because that identity is the sync
// cursor key and the document id prefix.
func promptInstanceID(p *prompter, plugin string, existing []config.Instance) (string, error) {
	taken := func(ident string) bool {
		for _, instance := range existing {
			if instance.Ident() == ident {
				return true
			}
		}
		return false
	}
	if !taken(plugin) {
		return "", nil
	}

	id, err := p.required("sources[].id", "sources already has an instance called "+plugin+
		", so this one needs its own id, for example "+plugin+"-2")
	if err != nil {
		return "", err
	}
	if taken(id) {
		return "", internalerror.NewBadRequestError("sources already has an instance called "+id+
			"; every id in sources must be unique", nil)
	}
	return id, nil
}

// secretHolds describes the credential a secret prompt is asking the variable
// name for. It is spelled from the manifest so the question names the
// operator's own system rather than a plugin this file would have to know.
func secretHolds(m lore.Manifest, secret lore.Secret) string {
	return m.Name + " " + strings.ReplaceAll(secret.Key, "_", " ")
}

// promptField asks for one declared field and reports whether it was answered:
// an optional field left empty stays out of the file entirely, so the plugin's
// own default keeps applying rather than being frozen into a configuration.
func promptField(p *prompter, field string, declared lore.Field) (any, bool, error) {
	question := declared.Prompt
	if question == "" {
		question = declared.Name
	}

	if declared.Type == lore.FieldStringList {
		if declared.Required {
			items, err := p.requiredList(field, question)
			return items, err == nil, err
		}
		items, err := p.list(question)
		return items, err == nil && len(items) > 0, err
	}

	answer, err := p.answer(field, question, declared)
	if err != nil || answer == "" {
		return nil, false, err
	}
	value, err := parseField(field, declared, answer)
	return value, err == nil, err
}

func parseField(field string, declared lore.Field, answer string) (any, error) {
	switch declared.Type {
	case lore.FieldURL:
		if err := validateURL(field, answer, declared.Default); err != nil {
			return nil, err
		}
		return answer, nil
	case lore.FieldInt:
		number, err := strconv.Atoi(answer)
		if err != nil {
			return nil, internalerror.NewBadRequestError(field+" must be a whole number, got "+
				strconv.Quote(answer), nil)
		}
		return number, nil
	case lore.FieldBool:
		switch strings.ToLower(answer) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, internalerror.NewBadRequestError(field+" must be true or false, got "+
			strconv.Quote(answer), nil)
	case lore.FieldDuration:
		if _, err := lore.ParseDuration(answer); err != nil {
			return nil, internalerror.NewBadRequestError(field+" must be a duration like 30m or 30d, got "+
				strconv.Quote(answer), nil)
		}
		// The answer is written back as text: the whole-day "30d" form the
		// configuration accepts survives no time.Duration round trip.
		return answer, nil
	default:
		return answer, nil
	}
}

type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func (p *prompter) ask(question, fallback string) (string, error) {
	if fallback == "" {
		_, _ = fmt.Fprintf(p.out, "%s: ", question)
	} else {
		_, _ = fmt.Fprintf(p.out, "%s [%s]: ", question, fallback)
	}

	line, err := p.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", internalerror.NewInternalError("cannot read the answer to "+question, err)
	}
	if answer := strings.TrimSpace(line); answer != "" {
		return answer, nil
	}
	return fallback, nil
}

func (p *prompter) envName(field, holds, fallback string) (string, error) {
	answer, err := p.ask("name of the environment variable holding the "+holds+" — the name, never the value", fallback)
	if err != nil {
		return "", err
	}
	if !envNamePattern.MatchString(answer) {
		// The answer is never echoed: a user who pastes a token here must not see it logged back.
		refusal := field + " must be an environment variable name"
		if fallback != "" {
			refusal += " like " + fallback
		}
		return "", internalerror.NewBadRequestError(refusal, nil)
	}
	return answer, nil
}

func (p *prompter) required(field, question string) (string, error) {
	answer, err := p.ask(question, "")
	if err != nil {
		return "", err
	}
	if answer == "" {
		return "", internalerror.NewBadRequestError(field+" must be set", nil)
	}
	return answer, nil
}

// answer offers the declared default when there is one, and otherwise refuses an
// empty answer to a required field: `source add` writing a configuration that
// every later `lore` invocation rejects at load is worse than asking again.
func (p *prompter) answer(field, question string, declared lore.Field) (string, error) {
	if declared.Required && declared.Default == "" {
		return p.required(field, question)
	}
	return p.ask(question, declared.Default)
}

func (p *prompter) list(question string) ([]string, error) {
	answer, err := p.ask(question, "")
	if err != nil {
		return nil, err
	}

	var items []string
	for _, item := range strings.Split(answer, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items, nil
}

// requiredList refuses an empty answer: a source that names no project would
// pass `source add` and then fail every later `lore` invocation at config load.
func (p *prompter) requiredList(field, question string) ([]string, error) {
	items, err := p.list(question)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, internalerror.NewBadRequestError(field+" must list at least one entry", nil)
	}
	return items, nil
}

// An unset optional URL means the plugin's own default, so only a value that was
// actually given has to be one a request can be built from.
func validateURL(field, raw, example string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return internalerror.NewBadRequestError(field+" is not a URL: "+raw, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		refusal := field + " must be an absolute http(s) URL"
		if example != "" {
			refusal += " like " + example
		}
		return internalerror.NewBadRequestError(refusal+", got "+raw, nil)
	}
	return nil
}

// encodeBlock renders the draft as one sequence item at the block's own
// indentation, id first: an instance is read by its identity, so the identity
// belongs on the line the reader's eye lands on.
func encodeBlock(indent string, draft sourceDraft) (string, error) {
	var block strings.Builder
	if draft.id != "" {
		block.WriteString(indent + "- id: " + draft.id + "\n")
		block.WriteString(indent + "  use: " + draft.use + "\n")
	} else {
		block.WriteString(indent + "- use: " + draft.use + "\n")
	}
	if len(draft.entries) == 0 {
		return block.String(), nil
	}

	// The keys sit two levels in from the item's dash, which is where `with:`
	// itself sits once the dash is counted as indentation.
	body := indent + "    "
	block.WriteString(indent + "  with:\n")
	for _, entry := range draft.entries {
		items, isList := entry.value.([]string)
		if !isList {
			value, err := encodeScalar(entry.key, entry.value)
			if err != nil {
				return "", err
			}
			block.WriteString(body + entry.key + ": " + value + "\n")
			continue
		}
		block.WriteString(body + entry.key + ":\n")
		for _, item := range items {
			value, err := encodeScalar(entry.key, item)
			if err != nil {
				return "", err
			}
			block.WriteString(body + "  - " + value + "\n")
		}
	}
	return block.String(), nil
}

// encodeScalar quotes an answer the way YAML needs it, because an answer that
// happens to hold a colon or a hash would otherwise change the document's shape.
func encodeScalar(key string, value any) (string, error) {
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return "", internalerror.NewInternalError("cannot encode the answer for "+key, err)
	}
	scalar := strings.TrimSuffix(string(encoded), "\n")
	if strings.Contains(scalar, "\n") {
		// A long answer is folded across lines by the encoder, which would break
		// the splice; a double-quoted scalar says the same thing on one line.
		if text, isText := value.(string); isText {
			return strconv.Quote(text), nil
		}
		return "", internalerror.NewInternalError("cannot encode the answer for "+key+" on one line", nil)
	}
	return scalar, nil
}

// insertSource appends the instance to the sources: block as text. lore.yaml is
// hand-written: a YAML round trip would reflow it and drop its comments.
func insertSource(content string, draft sourceDraft) (string, error) {
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	block := findSourcesBlock(content)
	if block.found && !block.spliceable && !block.flowEmpty {
		return "", internalerror.NewPreconditionError(sourcesKey+" in the configuration carries an inline"+
			" value, so an instance cannot be appended to it — rewrite it as a block sequence of items,"+
			" or add the instance by hand", nil)
	}

	item, err := encodeBlock(block.indent, draft)
	if err != nil {
		return "", err
	}
	switch {
	case !block.found:
		return content + sourcesKey + "\n" + item, nil
	case block.flowEmpty:
		// `sources: []` says "no instances" in a shape nothing can be appended
		// to, so the key is reopened as a block before the item goes under it.
		reopened := content[:block.keyEnd]
		if block.comment != "" {
			reopened += " " + block.comment
		}
		return reopened + "\n" + item + content[block.lineEnd:], nil
	default:
		return content[:block.end] + item + content[block.end:], nil
	}
}

// sourcesBlock is where a new item goes and how it must be written to fit.
type sourcesBlock struct {
	found      bool
	spliceable bool   // the key carries no inline value, so items can be appended
	flowEmpty  bool   // `sources: []`: the key must be reopened before an item fits
	comment    string // a trailing comment on the key's line, preserved when it is reopened
	keyEnd     int    // just past the key itself
	lineEnd    int    // just past the key's line
	end        int    // where the new item is spliced in
	indent     string // the indentation the block's items sit at
}

// findSourcesBlock walks the file rather than parsing it, because the offsets it
// reports are into text nobody is allowed to rewrite. A trailing comment is
// harmless above a block child; any other inline value is not.
//
// The block ends at its last content line, so trailing blank lines and comments
// stay below the new item: a comment there introduces what follows it.
func findSourcesBlock(content string) sourcesBlock {
	block := sourcesBlock{indent: sequenceIndent}

	offset, itemFound := 0, false
	for _, line := range strings.SplitAfter(content, "\n") {
		start := offset
		offset += len(line)
		body := strings.TrimRight(line, " \t\r\n")

		if !block.found {
			rest, isKey := strings.CutPrefix(body, sourcesKey)
			if !isKey {
				continue
			}
			value, comment := splitInlineComment(rest)
			block.found, block.comment = true, comment
			block.spliceable, block.flowEmpty = value == "", value == "[]"
			block.keyEnd, block.lineEnd, block.end = start+len(sourcesKey), offset, offset
			continue
		}

		trimmed := strings.TrimSpace(body)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(body, " ") && !strings.HasPrefix(body, "\t") {
			break // a sibling key at the top level: the block ended above it.
		}
		block.end = offset
		if indent, isItem := itemIndent(body); isItem && !itemFound {
			block.indent, itemFound = indent, true
		}
	}
	return block
}

// itemIndent reports the indentation of a sequence item, which is what a new
// item has to match: YAML reads an item indented differently from its siblings
// as a nested sequence.
func itemIndent(body string) (string, bool) {
	indent := body[:len(body)-len(strings.TrimLeft(body, " \t"))]
	rest := body[len(indent):]
	return indent, rest == "-" || strings.HasPrefix(rest, "- ")
}

// A hash anywhere in an inline value is either a comment or something this
// command must refuse, so splitting on the first one is enough.
func splitInlineComment(rest string) (value, comment string) {
	if at := strings.Index(rest, "#"); at >= 0 {
		return strings.TrimSpace(rest[:at]), strings.TrimSpace(rest[at:])
	}
	return strings.TrimSpace(rest), ""
}

func replaceFile(path, content string) error {
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return internalerror.NewInternalError("cannot write "+path, err)
	}
	written := func() error {
		if _, err := temp.WriteString(content); err != nil {
			return err
		}
		if err := temp.Chmod(mode); err != nil {
			return err
		}
		return temp.Close()
	}()
	if written != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		return internalerror.NewInternalError("cannot write "+path, written)
	}

	if err := os.Rename(temp.Name(), path); err != nil {
		_ = os.Remove(temp.Name())
		return internalerror.NewInternalError("cannot write "+path, err)
	}
	return nil
}
