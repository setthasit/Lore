package ollama

import (
	"fmt"
	"strings"

	"github.com/setthasit/Lore/sdk"
)

// Plugin is the official Ollama provider plugin.
func Plugin() lore.ProviderPlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:         "ollama",
		Kind:         lore.KindProvider,
		APIVersion:   lore.APIVersion,
		Summary:      "Local Ollama daemon embeddings and chat completions",
		Capabilities: lore.Capabilities{Embed: true, Complete: true},
		// Both suggestions are models the daemon pulls by name; the embedding
		// one still needs an explicit width, because no Ollama model implies one.
		DefaultModels: map[lore.Capability]string{
			lore.CapabilityEmbed:    "nomic-embed-text",
			lore.CapabilityComplete: "llama3.1",
		},
		Fields: []lore.Field{
			{
				Name:    "base_url",
				Type:    lore.FieldURL,
				Default: DefaultBaseURL,
				Doc:     "override to reach a daemon on another host or port",
				Prompt:  "Ollama daemon URL",
			},
		},
		// No secrets: the daemon is unauthenticated, and the manifest is the
		// only place that can say so — a provider with no declared secret is
		// one the host never resolves an environment variable for.
	}
}

// NewProvider builds only the half it was asked for: an embedding model and a
// chat model are separate connections configured from different bindings, so
// building both would demand configuration for a role nobody asked this
// instance to play.
func (p plugin) NewProvider(c lore.ProviderConfig) (lore.Provider, error) {
	var cfg struct {
		BaseURL string `json:"base_url"`
	}
	if err := c.Decode(&cfg); err != nil {
		return nil, err
	}

	switch c.Capability {
	case lore.CapabilityEmbed:
		if c.Dimensions <= 0 {
			return nil, fmt.Errorf("ollama: embedder.dimensions must be set to the vector width of %s: an Ollama model does not imply one; `ollama show %s` reports it", c.Model, c.Model)
		}
		return NewEmbedder(c.Model, cfg.BaseURL, c.Dimensions)
	case lore.CapabilityComplete:
		return New(c.Model, cfg.BaseURL)
	default:
		return nil, unsupportedCapability(c.Capability, p.Manifest().Capabilities)
	}
}

// unsupportedCapability names both sides, because the operator bound a role to
// this provider and the fix is one of the roles it does declare.
func unsupportedCapability(want lore.Capability, have lore.Capabilities) error {
	declared := have.Names()
	names := make([]string, len(declared))
	for i, name := range declared {
		names[i] = string(name)
	}
	return fmt.Errorf("ollama: capability %s is not served by this provider; it serves %s", want, strings.Join(names, ", "))
}
