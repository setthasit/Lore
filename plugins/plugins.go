package plugins

import (
	"github.com/setthasit/Lore/plugins/code/git"
	"github.com/setthasit/Lore/plugins/providers/anthropic"
	"github.com/setthasit/Lore/plugins/providers/compat"
	"github.com/setthasit/Lore/plugins/providers/ollama"
	"github.com/setthasit/Lore/plugins/providers/openai"
	"github.com/setthasit/Lore/plugins/sources/github"
	"github.com/setthasit/Lore/plugins/sources/gitlab"
	"github.com/setthasit/Lore/plugins/sources/jira"
	"github.com/setthasit/Lore/plugins/sources/notion"
	"github.com/setthasit/Lore/sdk"
)

// `lore init` scaffolds a workspace from the first plugin of each kind, so this order is load-bearing.
func Official() []lore.Plugin {
	return []lore.Plugin{
		github.Plugin(),
		gitlab.Plugin(),
		jira.Plugin(),
		notion.Plugin(),

		openai.Plugin(),
		anthropic.Plugin(),
		compat.Plugin(),
		ollama.Plugin(),

		git.Plugin(),
	}
}
