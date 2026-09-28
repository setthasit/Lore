package ollama

import (
	"fmt"

	"github.com/setthasit/Lore/sdk"
)

func Plugin() lore.ProviderPlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:         "ollama",
		Kind:         lore.KindProvider,
		APIVersion:   lore.APIVersion,
		Summary:      "Local Ollama daemon embeddings and chat completions",
		Capabilities: lore.Capabilities{Embed: true, Complete: true},
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
		// No secrets: the daemon is unauthenticated.
	}
}

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
		return nil, fmt.Errorf("ollama: capability %s is not served by this provider; it serves %s", c.Capability, p.Manifest().Capabilities)
	}
}
