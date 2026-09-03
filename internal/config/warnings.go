package config

import (
	"slices"
	"strings"
)

// Forges are the only sources whose documents carry a repository path a local
// clone can be mapped onto; a Notion page or a Jira ticket names none.
const (
	githubSource = "github"
	gitlabSource = "gitlab"
)

// StartupWarnings reports configuration that degrades answers without being an
// error.
func (c *Config) StartupWarnings() []string {
	var warnings []string
	for i := range c.Repos {
		repo := c.Repos[i]
		if repo.Remote == "" || c.ingestsRemote(repo.Remote) {
			continue
		}
		warnings = append(warnings, "repos path "+repo.Path+" has remote "+repo.Remote+
			", which names no configured source repo — blame still works, but chains stop at the commit layer")
	}
	return warnings
}

func (c *Config) ingestsRemote(remote string) bool {
	source, path, ok := strings.Cut(remote, ":")
	if !ok || !namespacedPath(path) {
		return false
	}

	switch source {
	case githubSource:
		// GitHub owner and repository names are case-insensitive.
		return c.Sources.GitHub != nil && slices.ContainsFunc(c.Sources.GitHub.Repos,
			func(ingested string) bool { return strings.EqualFold(ingested, path) })
	case gitlabSource:
		// A GitLab path is matched verbatim, and may nest through subgroups.
		return c.Sources.GitLab != nil && slices.Contains(c.Sources.GitLab.Projects, path)
	}
	return false
}

// A repository path is at least a namespace and a name. Depth beyond that is the
// forge's business: an entry that no configured source lists still warns.
func namespacedPath(path string) bool {
	segments := strings.Split(path, "/")
	return len(segments) >= 2 && !slices.Contains(segments, "")
}
