package git

import (
	"errors"

	"github.com/setthasit/Lore/sdk"
)

func Plugin() lore.CodePlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       "git",
		Kind:       lore.KindCode,
		APIVersion: lore.APIVersion,
		Summary:    "Blame and history for one local git clone (read-only)",
	}
}

// An empty root would run every git command against the process's working directory.
func (plugin) NewCode(c lore.CodeConfig) (lore.CodeRepo, error) {
	if c.Root == "" {
		return nil, errors.New("git: root is empty")
	}
	return New(c.Root), nil
}
