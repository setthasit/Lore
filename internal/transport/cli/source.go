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
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"lore/internal/config"
	"lore/internal/errors/internalerror"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type sourceSpec struct {
	name       string
	configured func(config.Sources) bool
	prompt     func(*prompter) (block string, envNames []string, err error)
}

var sourceSpecs = []sourceSpec{
	{name: "notion", configured: func(s config.Sources) bool { return s.Notion != nil }, prompt: promptNotion},
	{name: "jira", configured: func(s config.Sources) bool { return s.Jira != nil }, prompt: promptJira},
}

func newSourceCommand(configPath *string) *cobra.Command {
	source := &cobra.Command{
		Use:   "source",
		Short: "Manage the sources lore.yaml ingests",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	source.AddCommand(newSourceAddCommand(configPath))
	return source
}

func newSourceAddCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "add <" + strings.Join(sourceNames(), "|") + ">",
		Short: "Append a source block to lore.yaml, asking for its fields",
		Long: "Asks for the fields the source needs and inserts them under sources: in\n" +
			"lore.yaml, leaving every existing line untouched. It asks for the NAME of\n" +
			"the environment variable holding each credential, never the credential.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSourceAdd(cmd, args, *configPath)
		},
	}
}

func runSourceAdd(cmd *cobra.Command, args []string, configPath string) error {
	spec, err := sourceToAdd(args)
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
	current, err := decodeConfig(bytes.NewReader(original))
	if err != nil {
		return internalerror.NewBadRequestError("cannot parse "+configPath, err)
	}
	if spec.configured(current.Sources) {
		return internalerror.NewPreconditionError("sources."+spec.name+" is already configured in "+configPath+" — edit that block directly", nil)
	}

	block, envNames, err := spec.prompt(&prompter{
		in:  bufio.NewReader(cmd.InOrStdin()),
		out: cmd.OutOrStdout(),
	})
	if err != nil {
		return err
	}

	updated, err := insertBlock(string(original), block)
	if err != nil {
		return err
	}
	if _, err := decodeConfig(strings.NewReader(updated)); err != nil {
		return internalerror.NewInternalError("the sources."+spec.name+" block does not fit "+configPath+", which is unchanged", err)
	}
	if err := replaceFile(configPath, updated); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	printfln(out, "added sources.%s to %s", spec.name, configPath)
	printfln(out, "next: export %s, then run `lore sync`", strings.Join(envNames, " and "))
	return nil
}

func sourceToAdd(args []string) (*sourceSpec, error) {
	accepted := strings.Join(sourceNames(), " or ")
	if len(args) == 0 {
		return nil, internalerror.NewBadRequestError("name the source to add: "+accepted, nil)
	}
	if args[0] == "github" {
		return nil, internalerror.NewBadRequestError("`lore init` already scaffolds sources.github — edit that block in lore.yaml; source add takes "+accepted, nil)
	}
	for i := range sourceSpecs {
		if sourceSpecs[i].name == args[0] {
			return &sourceSpecs[i], nil
		}
	}
	return nil, internalerror.NewBadRequestError("unknown source "+args[0]+" — add "+accepted, nil)
}

func sourceNames() []string {
	names := make([]string, len(sourceSpecs))
	for i := range sourceSpecs {
		names[i] = sourceSpecs[i].name
	}
	return names
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
		return "", internalerror.NewBadRequestError(field+" must be an environment variable name like "+fallback, nil)
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

type notionBlock struct {
	TokenEnv  string   `yaml:"token_env"`
	RootPages []string `yaml:"root_pages,omitempty"`
}

type jiraBlock struct {
	BaseURL  string   `yaml:"base_url"`
	EmailEnv string   `yaml:"email_env"`
	TokenEnv string   `yaml:"token_env"`
	Projects []string `yaml:"projects,omitempty"`
}

func promptNotion(p *prompter) (string, []string, error) {
	tokenEnv, err := p.envName("sources.notion.token_env", "Notion integration token", "LORE_NOTION_TOKEN")
	if err != nil {
		return "", nil, err
	}
	rootPages, err := p.list("Notion root pages to scope the sync to, comma-separated (empty syncs every page shared with the integration)")
	if err != nil {
		return "", nil, err
	}

	block, err := encodeBlock("notion", notionBlock{TokenEnv: tokenEnv, RootPages: rootPages})
	if err != nil {
		return "", nil, err
	}
	return block, []string{tokenEnv}, nil
}

func promptJira(p *prompter) (string, []string, error) {
	baseURL, err := p.required("sources.jira.base_url", "Jira base URL, e.g. https://acme.atlassian.net")
	if err != nil {
		return "", nil, err
	}
	if err := validateBaseURL(baseURL); err != nil {
		return "", nil, err
	}
	emailEnv, err := p.envName("sources.jira.email_env", "Jira account email", "LORE_JIRA_EMAIL")
	if err != nil {
		return "", nil, err
	}
	tokenEnv, err := p.envName("sources.jira.token_env", "Jira API token", "LORE_JIRA_TOKEN")
	if err != nil {
		return "", nil, err
	}
	projects, err := p.list("Jira project keys to sync, comma-separated (empty syncs every project the account can see)")
	if err != nil {
		return "", nil, err
	}

	block, err := encodeBlock("jira", jiraBlock{
		BaseURL:  baseURL,
		EmailEnv: emailEnv,
		TokenEnv: tokenEnv,
		Projects: projects,
	})
	if err != nil {
		return "", nil, err
	}
	return block, []string{emailEnv, tokenEnv}, nil
}

func validateBaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return internalerror.NewBadRequestError("sources.jira.base_url is not a URL: "+raw, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return internalerror.NewBadRequestError("sources.jira.base_url must be an absolute http(s) URL like https://acme.atlassian.net, got "+raw, nil)
	}
	return nil
}

func encodeBlock(name string, fields any) (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(map[string]any{name: fields}); err != nil {
		return "", internalerror.NewInternalError("cannot encode the "+name+" block", err)
	}
	if err := encoder.Close(); err != nil {
		return "", internalerror.NewInternalError("cannot encode the "+name+" block", err)
	}

	var block strings.Builder
	for _, line := range strings.SplitAfter(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		block.WriteString("  " + line)
	}
	block.WriteString("\n")
	return block.String(), nil
}

// lore.yaml is hand-written: a YAML round trip would reflow it and drop its comments.
func insertBlock(content, block string) (string, error) {
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	at, spliceable, found := sourcesLineEnd(content)
	switch {
	case found && !spliceable:
		return "", internalerror.NewPreconditionError("sources: in the configuration carries an inline value, so a nested source cannot be added to it — rewrite it as a block mapping or add the source by hand", nil)
	case found:
		return content[:at] + block + content[at:], nil
	}
	return content + "sources:\n" + block, nil
}

// A trailing comment is harmless above a block child; any other inline value is not.
func sourcesLineEnd(content string) (end int, spliceable, found bool) {
	offset := 0
	for _, line := range strings.SplitAfter(content, "\n") {
		offset += len(line)
		rest, isKey := strings.CutPrefix(strings.TrimRight(line, " \t\r\n"), "sources:")
		if !isKey {
			continue
		}
		rest = strings.TrimSpace(rest)
		return offset, rest == "" || strings.HasPrefix(rest, "#"), true
	}
	return 0, false, false
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

func decodeConfig(raw io.Reader) (*config.Config, error) {
	decoder := yaml.NewDecoder(raw)
	decoder.KnownFields(true)

	var cfg config.Config
	if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &cfg, nil
}
