// Package plugins lists the plugins Lore ships. An official plugin has no
// privilege a third-party plugin lacks: it is registered through the same
// contract, validated by the same registry and configured the same way, and
// this list is only the argument a distribution happens to pass by default.
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

// Official returns every plugin this distribution ships, grouped by kind:
// sources, then providers, then code.
//
// The order is load-bearing in two ways, which is why it is fixed here rather
// than sorted anywhere else. `lore plugin list` prints it, so it must not churn
// between builds. And `lore init` scaffolds a fresh workspace from the FIRST
// plugin of each kind, so this list is where a distribution states its
// preference: the engine refuses to prefer a plugin by name, and the
// composition root is the only place allowed to. GitHub leads the sources
// because a public repository is the one source a new user can point at without
// provisioning anything, and OpenAI leads the providers because its embedding
// models imply their own vector width — an operator can start without knowing
// what `dimensions` means.
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
