package gitlab

import "github.com/setthasit/Lore/sdk"

var _ lore.SourcePlugin = plugin{}

// Plugin is the official GitLab source plugin.
func Plugin() lore.SourcePlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:         forgeName,
		Kind:         lore.KindSource,
		APIVersion:   lore.APIVersion,
		Summary:      "GitLab commits, merge requests, review threads, issues and notes (read-only)",
		Capabilities: lore.Capabilities{RepoRemotes: true},
		Fields: []lore.Field{
			{
				Name:    "base_url",
				Type:    lore.FieldURL,
				Default: DefaultBaseURL,
				Prompt:  "GitLab instance URL",
				Doc:     "Instance root. A self-managed instance names its own, \"https://gitlab.acme.dev\"; gitlab.com needs no entry.",
			},
			{
				Name:     "projects",
				Type:     lore.FieldStringList,
				Required: true,
				Prompt:   "Namespaced project paths to ingest, e.g. acme/myproject",
				Doc:      "Namespaced paths, matched verbatim: \"acme/myproject\", or \"acme/platform/myproject\" when the project nests through subgroups.",
			},
		},
		Secrets: []lore.Secret{
			{
				Key:         "token",
				ConfigField: "token_env",
				DefaultEnv:  "LORE_GITLAB_TOKEN",
				Doc:         "Personal or group access token with read_api scope.",
			},
		},
	}
}

func (plugin) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	var cfg struct {
		BaseURL  string   `json:"base_url"`
		Projects []string `json:"projects"`
	}
	if err := c.Decode(&cfg); err != nil {
		return nil, err
	}
	return NewConnector(c.Instance, c.Secret("token"), cfg.Projects, cfg.BaseURL), nil
}
