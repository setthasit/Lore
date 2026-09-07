package notion

import "github.com/setthasit/Lore/sdk"

func Plugin() lore.SourcePlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       "notion",
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "Notion pages and their block content (read-only)",
		Fields: []lore.Field{{
			Name:   "root_pages",
			Type:   lore.FieldStringList,
			Prompt: "Root pages to ingest, each a page id or an exact page title (empty syncs everything)",
			Doc:    "Each entry is a page id or an exact page title; the sync covers those pages and their descendants. An empty list syncs every page shared with the integration.",
		}},
		Secrets: []lore.Secret{{
			Key:         "token",
			ConfigField: "token_env",
			DefaultEnv:  "LORE_NOTION_TOKEN",
			Doc:         "Notion internal integration token; the integration must be shared with the pages to ingest.",
		}},
	}
}

func (plugin) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	var cfg struct {
		RootPages []string `json:"root_pages"`
	}
	if err := c.Decode(&cfg); err != nil {
		return nil, err
	}
	// Empty base URL means the one real Notion endpoint; only tests redirect it.
	return NewConnector(c.Instance, c.Secret("token"), cfg.RootPages, ""), nil
}
