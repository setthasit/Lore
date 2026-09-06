package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/sdk"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const sourcesKey = "sources"

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

	original, current, err := config.ReadFile(configPath)
	if err != nil {
		return err
	}

	draft, err := promptSource(&prompter{
		in:  bufio.NewReader(cmd.InOrStdin()),
		out: cmd.OutOrStdout(),
	}, manifest, current)
	if err != nil {
		return err
	}

	block, err := config.FindBlock(original, sourcesKey)
	if err != nil {
		return err
	}
	updated, err := block.AppendItem(draft.fields())
	if err != nil {
		return err
	}
	if err := config.WriteFile(configPath, updated, "the "+draft.ident()+" instance does not fit "+
		configPath+", which is unchanged"); err != nil {
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
	entries   []config.Field
	variables []string
}

func (d sourceDraft) ident() string {
	if d.id != "" {
		return d.id
	}
	return d.use
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
		draft.entries = append(draft.entries, config.Field{Key: secret.ConfigField, Value: name})
		draft.variables = append(draft.variables, name)
	}
	for _, declared := range m.Fields {
		value, set, err := promptField(p, field+declared.Name, declared)
		if err != nil {
			return draft, err
		}
		if set {
			draft.entries = append(draft.entries, config.Field{Key: declared.Name, Value: value})
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
		if err := registry.CheckURL(field, answer, declared.Default); err != nil {
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

func (d sourceDraft) fields() []config.Field {
	fields := make([]config.Field, 0, 3)
	if d.id != "" {
		fields = append(fields, config.Field{Key: "id", Value: d.id})
	}
	fields = append(fields, config.Field{Key: "use", Value: d.use})
	if len(d.entries) == 0 {
		return fields
	}
	return append(fields, config.Field{Key: "with", Value: d.entries})
}
