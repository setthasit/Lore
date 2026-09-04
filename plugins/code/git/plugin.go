package git

import (
	"errors"

	"github.com/setthasit/Lore/sdk"
)

// Plugin is the official git code plugin.
func Plugin() lore.CodePlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       "git",
		Kind:       lore.KindCode,
		APIVersion: lore.APIVersion,
		Summary:    "Blame and history for one local git clone (read-only)",
		// No fields and no secrets: a clone is identified by the path the host
		// already resolved, and reading it needs no credential.
	}
}

// NewCode refuses an empty root rather than building a repo bound to nothing,
// which would otherwise run every git command against the process's working
// directory — a different clone, answering plausible but wrong history.
func (plugin) NewCode(c lore.CodeConfig) (lore.CodeRepo, error) {
	if c.Root == "" {
		return nil, errors.New("git: root is empty")
	}
	return New(c.Root), nil
}
