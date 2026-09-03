package config

import (
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// Validate reports the first rule the configuration breaks. Load applies
// defaults before calling it, so absent optional tuning is already filled in.
//
// The loopback/TLS rule is deliberately not enforced here: it depends on the
// address `lore serve` actually binds and is ValidateListenAddr's job.
func (c *Config) Validate() error {
	if c.Workspace == "" {
		return internalerror.NewBadRequestError("workspace must be set", nil)
	}
	if c.Sources.none() && len(c.Repos) == 0 {
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
	if err := c.validateScheduler(); err != nil {
		return err
	}
	if c.LLM != nil && c.LLM.APIKeyEnv != "" {
		return requireEnvValue("llm.api_key_env", c.LLM.APIKeyEnv)
	}
	return nil
}

// Serving in the clear needs an addr that is provably loopback: a host name is never
// proof, it is not resolved here, and an empty host as in ":8080" reaches every interface.
func (c *Config) ValidateListenAddr(setting, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return internalerror.NewBadRequestError(
			setting+" must be a host:port address, got "+strconv.Quote(addr), err)
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
		return nil
	}
	if c.Server.MTLS != nil && c.Server.MTLS.Cert != "" && c.Server.MTLS.Key != "" {
		return nil
	}
	return internalerror.NewBadRequestError(setting+" "+strconv.Quote(addr)+
		" is not a loopback address, so it must be served over TLS: set both server.mtls.cert"+
		" and server.mtls.key, or bind 127.0.0.1:"+port, nil)
}

func (c *Config) validateSources() error {
	if github := c.Sources.GitHub; github != nil {
		if err := requireEnvValue("sources.github.token_env", github.TokenEnv); err != nil {
			return err
		}
	}
	if gitlab := c.Sources.GitLab; gitlab != nil {
		// base_url is optional here, unlike Jira: gitlab.com is the default instance.
		if err := requireAbsoluteURL("sources.gitlab.base_url", gitlab.BaseURL, "https://gitlab.com"); err != nil {
			return err
		}
		if err := requireEnvValue("sources.gitlab.token_env", gitlab.TokenEnv); err != nil {
			return err
		}
		if len(gitlab.Projects) == 0 {
			return internalerror.NewBadRequestError(
				`sources.gitlab.projects must list at least one "group/project" path`, nil)
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

// Sources is a struct of optional pointers, so "no source at all" has to be
// spelled out; forgetting a new one here would silently accept an empty workspace.
func (s Sources) none() bool {
	return s.GitHub == nil && s.GitLab == nil && s.Notion == nil && s.Jira == nil
}

// An unset optional base_url means the provider's own host, so only a value that
// is present has to be a URL a request can actually be built from.
func requireAbsoluteURL(field, raw, example string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return internalerror.NewBadRequestError(field+" is not a URL: "+raw, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return internalerror.NewBadRequestError(
			field+" must be an absolute http(s) URL like "+example+", got "+raw, nil)
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

func (c *Config) validateScheduler() error {
	if c.Scheduler.Interval < 0 {
		return internalerror.NewBadRequestError("scheduler.interval must not be negative, got "+c.Scheduler.Interval.String(), nil)
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
