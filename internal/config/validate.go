package config

import (
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

func (c *Config) Validate() error {
	if c.Workspace == "" {
		return internalerror.NewBadRequestError("workspace must be set", nil)
	}
	if len(c.Sources) == 0 && len(c.Repos) == 0 {
		return internalerror.NewBadRequestError("at least one of sources or repos must be configured", nil)
	}
	if err := validateInstances("sources", c.Sources); err != nil {
		return err
	}
	if err := validateInstances("providers", c.Providers); err != nil {
		return err
	}
	if err := c.validateEmbedder(); err != nil {
		return err
	}
	if err := c.validateRepos(); err != nil {
		return err
	}
	if err := c.validateQuery(); err != nil {
		return err
	}
	return c.validateScheduler()
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

// A duplicate identity would make two instances overwrite each other's documents.
func validateInstances(section string, instances []Instance) error {
	first := make(map[string]int, len(instances))
	for i := range instances {
		instance := instances[i]
		if instance.Use == "" {
			return internalerror.NewBadRequestError(
				section+"["+instanceLabel(instance, i)+"].use must be set", nil)
		}
		if earlier, taken := first[instance.Ident()]; taken {
			return duplicateInstance(section, instances[earlier], instance)
		}
		first[instance.Ident()] = i
	}
	return nil
}

func instanceLabel(instance Instance, index int) string {
	if ident := instance.Ident(); ident != "" {
		return ident
	}
	return strconv.Itoa(index)
}

func duplicateInstance(section string, earlier, current Instance) error {
	if earlier.ID == "" && current.ID == "" {
		return internalerror.NewBadRequestError(section+" lists "+strconv.Quote(current.Use)+
			" twice; give each instance a distinct id, for example id: "+current.Use+"-acme", nil)
	}
	return internalerror.NewBadRequestError(section+"["+current.Ident()+
		"] is declared twice; every id in "+section+" must be unique", nil)
}

func (c *Config) validateEmbedder() error {
	if c.Embedder.Provider == "" {
		return internalerror.NewBadRequestError("embedder.provider must be set", nil)
	}
	if c.Embedder.Dimensions < 0 {
		return internalerror.NewBadRequestError("embedder.dimensions must not be negative, got "+
			strconv.Itoa(c.Embedder.Dimensions), nil)
	}
	return nil
}

// An unmatched repos[].remote is a startup warning, not an error.
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
