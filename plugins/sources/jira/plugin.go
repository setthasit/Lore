package jira

import "github.com/setthasit/Lore/sdk"

var _ lore.SourcePlugin = plugin{}

// Plugin is the official Jira source plugin.
func Plugin() lore.SourcePlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       "jira",
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "Jira Cloud issues and their comments (read-only)",
		Fields: []lore.Field{
			{
				Name:     "base_url",
				Type:     lore.FieldURL,
				Required: true,
				Prompt:   "Jira site URL",
				Doc:      "https://<org>.atlassian.net",
			},
			{
				Name:   "projects",
				Type:   lore.FieldStringList,
				Prompt: "Project keys to ingest (empty syncs everything)",
				Doc:    "Project keys such as PROJ. An empty list syncs every project the credentials can browse.",
			},
		},
		Secrets: []lore.Secret{
			{
				Key:         "email",
				ConfigField: "email_env",
				DefaultEnv:  "LORE_JIRA_EMAIL",
				Doc:         "Atlassian account email the API token belongs to; Jira Cloud authenticates the pair, not the token alone.",
			},
			{
				Key:         "token",
				ConfigField: "token_env",
				DefaultEnv:  "LORE_JIRA_TOKEN",
				Doc:         "Atlassian API token for that account.",
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
	return NewConnector(c.Instance, cfg.BaseURL, c.Secret("email"), c.Secret("token"), cfg.Projects), nil
}
