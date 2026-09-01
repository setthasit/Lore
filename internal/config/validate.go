package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"lore/internal/errors/internalerror"
)

// Validate reports the first rule the configuration breaks. Load applies
// defaults before calling it, so absent optional tuning is already filled in.
//
// The loopback/TLS rule is deliberately not enforced here: it depends on the
// addresses `lore serve` actually binds and is checked at startup.
func (c *Config) Validate() error {
	if c.Workspace == "" {
		return internalerror.NewBadRequestError("workspace must be set", nil)
	}
	if c.Sources.GitHub == nil && c.Sources.Notion == nil && c.Sources.Jira == nil && len(c.Repos) == 0 {
		return internalerror.NewBadRequestError("at least one of sources or repos must be configured", nil)
	}
	if err := c.validateSources(); err != nil {
		return err
	}
	if err := c.validateRepos(); err != nil {
		return err
	}
	if err := c.validateQuery(); err != nil {
		return err
	}
	if c.LLM != nil && c.LLM.APIKeyEnv != "" {
		return requireEnvValue("llm.api_key_env", c.LLM.APIKeyEnv)
	}
	return nil
}

func (c *Config) validateSources() error {
	if github := c.Sources.GitHub; github != nil {
		if err := requireEnvValue("sources.github.token_env", github.TokenEnv); err != nil {
			return err
		}
	}
	if notion := c.Sources.Notion; notion != nil {
		if err := requireEnvValue("sources.notion.token_env", notion.TokenEnv); err != nil {
			return err
		}
	}
	if jira := c.Sources.Jira; jira != nil {
		if jira.BaseURL == "" {
			return internalerror.NewBadRequestError("sources.jira.base_url must be set", nil)
		}
		if err := requireEnvValue("sources.jira.email_env", jira.EmailEnv); err != nil {
			return err
		}
		if err := requireEnvValue("sources.jira.token_env", jira.TokenEnv); err != nil {
			return err
		}
	}
	return nil
}

// An unmatched repos[].remote is a StartupWarnings entry, not an error.
func (c *Config) validateRepos() error {
	for i := range c.Repos {
		path := c.Repos[i].Path
		if path == "" {
			return internalerror.NewBadRequestError("every entry in repos must have a path", nil)
		}

		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return internalerror.NewBadRequestError("repos path "+path+" does not exist", err)
		}
		if err != nil {
			return internalerror.NewBadRequestError("repos path "+path+" cannot be read", err)
		}
		if !info.IsDir() {
			return internalerror.NewBadRequestError("repos path "+path+" is not a directory", nil)
		}

		_, err = os.Stat(filepath.Join(path, ".git"))
		if errors.Is(err, fs.ErrNotExist) {
			return internalerror.NewBadRequestError("repos path "+path+" is not a git repository — no .git entry found", err)
		}
		if err != nil {
			return internalerror.NewBadRequestError("repos path "+path+" has a .git entry that cannot be read", err)
		}
	}
	return nil
}

func (c *Config) validateQuery() error {
	if c.Query.EventWindow < 0 {
		return internalerror.NewBadRequestError("query.event_window must not be negative, got "+c.Query.EventWindow.String(), nil)
	}
	if c.Query.WalkDepth < 0 {
		return internalerror.NewBadRequestError("query.walk_depth must not be negative", nil)
	}
	if c.Query.TopK < 0 {
		return internalerror.NewBadRequestError("query.top_k must not be negative", nil)
	}
	return nil
}

// requireEnvValue enforces that a configured *_env key names an environment
// variable that is actually set: secrets live in the environment only, so a
// missing one is a configuration error naming the variable the operator must set.
func requireEnvValue(field, name string) error {
	if name == "" {
		return internalerror.NewBadRequestError(field+" must name an environment variable", nil)
	}
	if os.Getenv(name) == "" {
		return internalerror.NewBadRequestError(field+" names "+name+", but that environment variable is not set", nil)
	}
	return nil
}
