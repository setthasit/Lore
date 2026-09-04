package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/sdk"
)

// commentColumn is where a trailing comment starts. The scaffold is read far
// more often than it is generated, so its comments line up in one column the
// way a hand-written file's would.
const commentColumn = 43

// The tuning blocks describe the engine's own behavior rather than any plugin's,
// so they are the one part of the scaffold no manifest can generate.
const (
	queryBlock = `# query:                                   # optional tuning
#   event_window: 30d                      # ± window for event resolution
#   walk_depth: 3
#   top_k: 12
`

	schedulerBlock = `# scheduler:
#   interval: 30m
`
)

func newInitCommand(configPath *string, reg *registry.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Write a lore.yaml scaffold for this workspace",
		Long: "Writes a commented lore.yaml next to you, generated from the manifests of\n" +
			"the plugins this build registers: a starter source instance to fill in and\n" +
			"the secret-variable names it reads. It never touches an index: `lore sync`\n" +
			"creates that on its first run.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, *configPath, reg)
		},
	}
}

func runInit(cmd *cobra.Command, configPath string, reg *registry.Registry) error {
	// The plan is resolved before the file exists, so a build that cannot
	// produce a loadable scaffold refuses instead of leaving an empty file.
	plan, err := newScaffold(reg, workspaceName(configPath))
	if err != nil {
		return err
	}

	// Check-then-write would overwrite a configuration written in between.
	file, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return internalerror.NewPreconditionError(configPath+" already exists — edit it, or pass --config to write elsewhere", nil)
		}
		return internalerror.NewInternalError("cannot create "+configPath, err)
	}

	if _, err := file.WriteString(plan.render()); err != nil {
		_ = file.Close()
		return internalerror.NewInternalError("cannot write "+configPath, err)
	}
	if err := file.Close(); err != nil {
		return internalerror.NewInternalError("cannot write "+configPath, err)
	}

	out := cmd.OutOrStdout()
	printfln(out, "wrote %s", configPath)
	if variables := plan.variables(); len(variables) > 0 {
		printfln(out, "next: fill in the fields it marks, export %s, then run `lore sync`",
			strings.Join(variables, " and "))
	} else {
		printfln(out, "next: fill in the fields it marks, then run `lore sync`")
	}
	return nil
}

func workspaceName(configPath string) string {
	dir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return "myproject"
	}
	name := filepath.Base(dir)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "myproject"
	}
	return name
}

// scaffold is the plugin set a fresh workspace is written for. Which plugins
// those are is the composition root's choice, expressed as registration order
// and read back through Starter: preferring one by name here would put a
// plugin's name back into the engine.
type scaffold struct {
	workspace string
	source    lore.Manifest
	embedder  lore.Manifest
	llm       lore.Manifest
	hasLLM    bool
}

func newScaffold(reg *registry.Registry, workspace string) (*scaffold, error) {
	source, ok := reg.Starter(lore.KindSource, "")
	if !ok {
		return nil, internalerror.NewPreconditionError("this build registers no source plugin, so there is"+
			" nothing for a workspace to ingest — run `lore plugin list` to see what it has", nil)
	}
	embedder, ok := reg.Starter(lore.KindProvider, lore.CapabilityEmbed)
	if !ok {
		return nil, internalerror.NewPreconditionError("this build registers no provider serving embeddings,"+
			" and a workspace without vectors has nothing to search — run `lore plugin list` to see what it has", nil)
	}

	plan := &scaffold{workspace: workspace, source: source, embedder: embedder}
	// Synthesis is optional, so a build with no completion provider still
	// scaffolds; it just has no stanza to suggest commenting back in.
	plan.llm, plan.hasLLM = reg.Starter(lore.KindProvider, lore.CapabilityComplete)
	return plan, nil
}

func (s *scaffold) render() string {
	var out strings.Builder

	out.WriteString("workspace: " + s.workspace + "\n\n")
	out.WriteString("# The index is derived data: safe to delete, rebuilt by the next lore sync.\n")
	out.WriteString("# index_path: ~/.lore/" + s.workspace + ".db\n\n")

	out.WriteString("# Sources say what to INGEST: one sequence item per instance, in sync order.\n")
	out.WriteString("# Secrets are never stored here: an *_env key names an environment variable.\n")
	out.WriteString("# `lore source add <plugin>` appends another instance; `lore plugin list` names them.\n")
	out.WriteString("sources:\n")
	out.WriteString(s.sourceItem())
	out.WriteString("\n")

	out.WriteString("# Local clones, for blame and file history only. Zero repos is a valid\n")
	out.WriteString("# ask-only workspace.\n")
	out.WriteString("repos: []\n")
	out.WriteString("# repos:\n")
	out.WriteString("#   - path: ~/dev/" + s.workspace + "\n")
	out.WriteString("#     remote: " + s.source.Name + ":acme/" + s.workspace + "\n\n")

	out.WriteString(queryBlock)
	out.WriteString("\n")
	out.WriteString(s.embedderBlock())
	out.WriteString("\n")
	if s.hasLLM {
		out.WriteString(s.llmBlock())
		out.WriteString("\n")
	}
	out.WriteString(schedulerBlock)
	return out.String()
}

func (s *scaffold) sourceItem() string {
	item := "  - use: " + s.source.Name + "\n"
	if body := withBlock(s.source, "      "); body != "" {
		item += "    with:\n" + body
	}
	return item
}

func (s *scaffold) embedderBlock() string {
	return "# Role binding: which provider instance embeds, and with which model.\n" +
		"embedder:\n" +
		scaffoldLine("  ", "provider: "+s.embedder.Name, credentialNote(s.embedder)) +
		scaffoldLine("  ", "model: "+scalar(s.embedder.DefaultModels[lore.CapabilityEmbed]),
			"changing the model needs: lore sync --reembed")
}

func (s *scaffold) llmBlock() string {
	return "# Synthesis: lore ask and --explain answer in prose only with this block.\n" +
		"# llm:\n" +
		scaffoldLine("#   ", "provider: "+s.llm.Name, credentialNote(s.llm)) +
		scaffoldLine("#   ", "model: "+scalar(s.llm.DefaultModels[lore.CapabilityComplete]), "")
}

// variables are the environment variables the scaffold's active stanzas name, in
// the order they appear in it. The commented llm: stanza is left out: nothing has
// to be exported for a workspace that is not synthesizing yet.
func (s *scaffold) variables() []string {
	var names []string
	for _, manifest := range []lore.Manifest{s.source, s.embedder} {
		for _, secret := range manifest.Secrets {
			if secret.DefaultEnv != "" && !slices.Contains(names, secret.DefaultEnv) {
				names = append(names, secret.DefaultEnv)
			}
		}
	}
	return names
}

// withBlock renders a plugin's `with:` block from its manifest: a secret becomes
// the key naming its variable, a required field a placeholder to fill in, and an
// optional field a commented line. Writing the optional keys out is what lets an
// operator see everything a plugin accepts without looking the plugin up.
func withBlock(m lore.Manifest, indent string) string {
	var out strings.Builder
	for _, secret := range m.Secrets {
		out.WriteString(scaffoldLine(indent, secret.ConfigField+": "+scalar(secret.DefaultEnv), secret.Doc))
	}
	for _, field := range m.Fields {
		entry := field.Name + ": " + placeholder(field)
		if !field.Required {
			entry = "# " + entry
		}
		out.WriteString(scaffoldLine(indent, entry, field.Doc))
	}
	return out.String()
}

// placeholder is a value of the field's type that parses but says nothing, so a
// scaffold loads while the workspace is still being filled in.
func placeholder(f lore.Field) string {
	switch f.Type {
	case lore.FieldStringList:
		return "[]"
	case lore.FieldInt:
		if f.Default == "" {
			return "0"
		}
		return f.Default
	case lore.FieldBool:
		if f.Default == "" {
			return "false"
		}
		return f.Default
	default:
		return scalar(f.Default)
	}
}

// A key with no value at all decodes as null, which a plugin's decoder then
// reports as a type error rather than as the blank it is.
func scalar(value string) string {
	if value == "" {
		return `""`
	}
	return value
}

// credentialNote names the variables a role binding's provider reads. A binding
// that names a plugin rather than a declared instance is built with that
// plugin's defaults, so the manifest's default variables are the ones in force.
func credentialNote(m lore.Manifest) string {
	var names []string
	for _, secret := range m.Secrets {
		if secret.DefaultEnv != "" {
			names = append(names, secret.DefaultEnv)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "credentials come from " + strings.Join(names, " and ")
}

func scaffoldLine(indent, body, comment string) string {
	text := indent + body
	if comment == "" {
		return text + "\n"
	}
	pad := max(commentColumn-len(text), 1)
	return text + strings.Repeat(" ", pad) + "# " + comment + "\n"
}
