package github

import "github.com/setthasit/Lore/sdk"

var _ lore.SourcePlugin = plugin{}

// Plugin is the official GitHub source plugin.
func Plugin() lore.SourcePlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       forgeName,
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "GitHub commits, pull requests, issues and their comments and reviews (read-only)",
		// Documents carry a "github:owner/name" RepoRef, so a registered local
		// clone can be matched against what this instance ingests.
		Capabilities: lore.Capabilities{RepoRemotes: true},
		Fields: []lore.Field{
			{
				Name:     "repos",
				Type:     lore.FieldStringList,
				Required: true,
				Prompt:   "Repositories to ingest, each \"owner/name\"",
				Doc:      "Each entry is \"owner/name\"; ingestion is over the API, so no local clone is needed.",
			},
		},
		Secrets: []lore.Secret{
			{
				Key:         "token",
				ConfigField: "token_env",
				DefaultEnv:  "LORE_GITHUB_TOKEN",
				Doc:         "A fine-grained personal access token with read-only access to the listed repositories.",
			},
		},
	}
}

func (plugin) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	var cfg struct {
		Repos []string `json:"repos"`
	}
	if err := c.Decode(&cfg); err != nil {
		return nil, err
	}
	// GitHub Enterprise Server roots are not part of the configuration surface
	// yet, so the connector's baseURL stays empty and means github.com.
	return NewConnector(c.Instance, c.Secret("token"), cfg.Repos, ""), nil
}
