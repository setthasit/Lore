package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/envx"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/sdk"
)

const sourcesKey = "sources"

const missingInstanceID = "sources[].id must be set"

const externalPromptNotice = "the questions below are the ones this plugin's own manifest declares;" +
	" answer each one with configuration; a secret's field holds the credential, either as the NAME" +
	" of the environment variable holding it, written as " + envx.Form + " so the value stays out of lore.yaml," +
	" or as the value itself, written into lore.yaml in plain text"

const (
	secretFromVariable = "env"
	secretFromValue    = "value"
)

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
			"untouched. A secret's field holds the credential: it asks whether to write\n" +
			envx.Form + ", naming the environment variable holding it, or the value\n" +
			"itself, which then lands in the file in plain text.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSourceAdd(cmd, args, *configPath, reg)
		},
	}
}

func sourceArgument(reg *registry.Registry) []string {
	if names := reg.Names(lore.KindSource); len(names) > 0 {
		return names
	}
	return []string{"plugin"}
}

func runSourceAdd(cmd *cobra.Command, args []string, configPath string, reg *registry.Registry) error {
	manifest, compiledIn, err := sourceToAdd(cmd.Context(), args, configPath, reg)
	if err != nil {
		return err
	}

	original, current, err := config.ReadFile(configPath)
	if err != nil {
		return err
	}
	refs, err := current.ExpandPluginRefs()
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if !compiledIn {
		printfln(out, "%s", externalPromptNotice)
	}
	draft, err := promptSource(&prompter{
		in:  bufio.NewReader(cmd.InOrStdin()),
		out: out,
	}, manifest, compiledIn, refs.Sources)
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
	if err := config.WriteFile(configPath, config.Splice{From: original, To: updated},
		"the "+draft.ident()+" instance does not fit "+configPath+", which is unchanged"); err != nil {
		return err
	}

	printfln(out, "added sources[%s] to %s", draft.ident(), configPath)
	if len(draft.variables) > 0 {
		printfln(out, "next: export %s, then run `lore sync`", strings.Join(draft.variables, " and "))
	} else {
		printfln(out, "next: run `lore sync`")
	}
	return nil
}

func sourceToAdd(ctx context.Context, args []string, configPath string, reg *registry.Registry) (lore.Manifest, bool, error) {
	if len(args) > 0 {
		if manifest, known := reg.Manifest(args[0]); known && manifest.Kind == lore.KindSource {
			return manifest, true, nil
		}
	}

	workspace, err := plugindist.Open(configPath, plugindist.WithHandshake(declaredManifest(reg.Log())))
	if err != nil {
		return lore.Manifest{}, false, err
	}
	if len(args) > 0 {
		manifest, err := installedExternal(ctx, workspace, args[0])
		if err != nil || manifest.Kind == lore.KindSource {
			return manifest, false, err
		}
	}
	return lore.Manifest{}, false, noSourceToAdd(args, addableSources(ctx, workspace, reg))
}

func installedExternal(ctx context.Context, workspace *plugindist.Workspace, name string) (lore.Manifest, error) {
	decl, declared := workspace.Declaration(name)
	if !declared {
		return lore.Manifest{}, nil
	}
	manifest, err := workspace.Manifest(ctx, decl)
	if err != nil {
		return lore.Manifest{}, err
	}
	if err := registry.CheckExternal(decl.Name, manifest); err != nil {
		return lore.Manifest{}, err
	}
	return manifest, nil
}

func addableSources(ctx context.Context, workspace *plugindist.Workspace, reg *registry.Registry) string {
	names := reg.Names(lore.KindSource)
	for _, decl := range workspace.Plugins() {
		if _, compiled := reg.Manifest(decl.Name); compiled {
			continue
		}
		if manifest, err := installedExternal(ctx, workspace, decl.Name); err == nil && manifest.Kind == lore.KindSource {
			names = append(names, decl.Name)
		}
	}

	if len(names) == 0 {
		return "no source plugin is registered or installed at all"
	}
	return "the source plugins you can add are " + strings.Join(names, ", ")
}

func noSourceToAdd(args []string, addable string) error {
	if len(args) == 0 {
		return internalerror.NewBadRequestError("name the source plugin to add: "+addable, nil)
	}
	return internalerror.NewBadRequestError("unknown source plugin "+args[0]+
		" — "+addable+"; run `lore plugin list` to see them all", nil)
}

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

func promptSource(p *prompter, m lore.Manifest, compiledIn bool, sources []config.Instance) (sourceDraft, error) {
	draft := sourceDraft{use: m.Name}

	id, err := promptInstanceID(p, m.Name, sources)
	if err != nil {
		return draft, err
	}
	draft.id = id

	field := "sources[" + draft.ident() + "].with."
	for _, secret := range m.Secrets {
		value, variable, err := p.secret(field+secret.Key, secretHolds(m, secret), defaultEnv(secret, compiledIn))
		if err != nil {
			return draft, err
		}
		draft.entries = append(draft.entries, config.Field{Key: secret.Key, Value: value})
		if variable != "" {
			draft.variables = append(draft.variables, variable)
		}
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

func defaultEnv(secret lore.Secret, compiledIn bool) string {
	if !compiledIn {
		return ""
	}
	return secret.DefaultEnv
}

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

	question := "sources already has an instance called " + plugin +
		", so this one needs its own id, for example " + plugin + "-2"
	for {
		id, atEOF, err := p.read(question, "")
		if err != nil {
			return "", err
		}
		if id == "" {
			if atEOF {
				return "", internalerror.NewBadRequestError(missingInstanceID, nil)
			}
			printfln(p.out, "%s", missingInstanceID)
			continue
		}
		if !registry.ValidInstanceID(id) {
			printfln(p.out, "%q cannot be an instance id: %s", id, registry.InstanceIDRule)
			continue
		}
		if taken(id) {
			printfln(p.out, "sources already has an instance called %s; every id in sources must be unique", id)
			continue
		}
		return id, nil
	}
}

func secretHolds(m lore.Manifest, secret lore.Secret) string {
	return m.Name + " " + strings.ReplaceAll(secret.Key, "_", " ")
}

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
		if err := registry.CheckURL(field, answer, inertLine(declared.Default)); err != nil {
			return nil, err
		}
		return answer, nil
	case lore.FieldInt:
		number, err := strconv.Atoi(answer)
		if err != nil {
			return nil, internalerror.NewBadRequestError(field+" must be a whole number", nil)
		}
		return number, nil
	case lore.FieldBool:
		switch strings.ToLower(answer) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, internalerror.NewBadRequestError(field+" must be true or false", nil)
	case lore.FieldDuration:
		if _, err := lore.ParseDuration(answer); err != nil {
			return nil, internalerror.NewBadRequestError(field+" must be a duration like 30m or 30d", nil)
		}
		// Written back as text: the whole-day "30d" form survives no time.Duration round trip.
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
	answer, _, err := p.read(question, fallback)
	if err != nil {
		return "", err
	}
	if answer == "" {
		return fallback, nil
	}
	return answer, nil
}

func (p *prompter) read(question, fallback string) (answer string, atEOF bool, err error) {
	asked := inertLine(question)
	if fallback == "" {
		_, _ = fmt.Fprintf(p.out, "%s: ", asked)
	} else {
		_, _ = fmt.Fprintf(p.out, "%s [%s]: ", asked, inertLine(fallback))
	}

	line, err := p.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, internalerror.NewInternalError("cannot read the answer to "+asked, err)
	}
	return strings.TrimSpace(line), errors.Is(err, io.EOF), nil
}

// variable is empty when the operator typed the value itself.
func (p *prompter) secret(field, holds, suggested string) (value, variable string, err error) {
	form, err := p.ask("the "+holds+": "+secretFromVariable+" reads it from an environment variable, written as "+
		envx.Form+"; "+secretFromValue+" writes it into lore.yaml as typed — "+
		secretFromVariable+" or "+secretFromValue, secretFromVariable)
	if err != nil {
		return "", "", err
	}
	switch strings.ToLower(form) {
	case secretFromVariable:
		variable, err = p.envName(field, holds, suggested)
		if err != nil {
			return "", "", err
		}
		return envx.Reference(variable), variable, nil
	case secretFromValue:
		value, err = p.literal(field, holds)
		if err != nil {
			return "", "", err
		}
		return envx.Escape(value), "", nil
	}
	// The answer is never echoed: a user who pastes a token here must not see it logged back.
	return "", "", internalerror.NewBadRequestError(
		field+" comes from "+secretFromVariable+" or "+secretFromValue+"; answer one of them", nil)
}

func (p *prompter) literal(field, holds string) (string, error) {
	printfln(p.out, "the %s will be written to lore.yaml in plain text", inertLine(holds))
	answer, err := p.ask("type the "+holds+" (it will not be printed back)", "")
	if err != nil {
		return "", err
	}
	if answer == "" {
		return "", internalerror.NewBadRequestError(
			field+" must be set: type the credential, or answer "+secretFromVariable+" to name a variable", nil)
	}
	return answer, nil
}

func (p *prompter) envName(field, holds, suggested string) (string, error) {
	answer, err := p.ask("name of the environment variable holding the "+holds+" — the name, never the value", suggested)
	if err != nil {
		return "", err
	}
	if !envx.ValidName(answer) {
		// The answer is never echoed: a user who pastes a token here must not see it logged back.
		refusal := field + " must be an environment variable name"
		if suggested != "" {
			refusal += " like " + suggested
		}
		return "", internalerror.NewBadRequestError(refusal+": "+envx.NameRule, nil)
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
