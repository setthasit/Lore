package config

import "strings"

const codeSource = "github"

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
	source, slug, ok := strings.Cut(remote, ":")
	if !ok || source != codeSource || c.Sources.GitHub == nil {
		return false
	}
	owner, name, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return false
	}

	// GitHub owner and repository names are case-insensitive.
	for _, ingested := range c.Sources.GitHub.Repos {
		if strings.EqualFold(ingested, slug) {
			return true
		}
	}
	return false
}
