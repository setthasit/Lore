package openai

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/setthasit/Lore/sdk"
)

// Plugin is the official OpenAI provider plugin.
func Plugin() lore.ProviderPlugin { return plugin{} }

type plugin struct{}

// modelDims is the vector width each embedding model produces. It lives with
// the driver that knows it: the host composes the vector-space identity from
// the width a provider reports, so a width decided anywhere else would be a
// guess about someone else's models.
var modelDims = map[string]int{
	"text-embedding-3-small": 1536,
	"text-embedding-3-large": 3072,
	"text-embedding-ada-002": 1536,
}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:         providerName,
		Kind:         lore.KindProvider,
		APIVersion:   lore.APIVersion,
		Summary:      "OpenAI embeddings and chat completions",
		Capabilities: lore.Capabilities{Embed: true, Complete: true},
		// The embedding suggestion is deliberately one whose width this driver
		// knows, so a scaffold copied unchanged produces a usable vector space.
		DefaultModels: map[lore.Capability]string{
			lore.CapabilityEmbed:    "text-embedding-3-small",
			lore.CapabilityComplete: "gpt-4o-mini",
		},
		Fields: []lore.Field{
			{
				Name:    "base_url",
				Type:    lore.FieldURL,
				Default: DefaultBaseURL,
				Doc:     "override to reach a gateway or a proxy in front of the API",
				Prompt:  "OpenAI API base URL",
			},
		},
		Secrets: []lore.Secret{
			{
				Key:         "api_key",
				ConfigField: "api_key_env",
				DefaultEnv:  "OPENAI_API_KEY",
				Doc:         "OpenAI API key, sent as a bearer token",
			},
		},
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
		dims, err := embedDimensions(c)
		if err != nil {
			return nil, err
		}
		return NewEmbedder(c.Secret("api_key"), c.Model, cfg.BaseURL, dims)
	case lore.CapabilityComplete:
		return New(c.Secret("api_key"), c.Model, cfg.BaseURL)
	default:
		return nil, unsupportedCapability(c.Capability, p.Manifest().Capabilities)
	}
}

// embedDimensions takes the width from the model rather than from the operator,
// and refuses a declared width instead of picking one of two answers that
// disagree: the wrong width poisons every vector written under it.
func embedDimensions(c lore.ProviderConfig) (int, error) {
	if c.Dimensions != 0 {
		return 0, errors.New("openai: embedder.dimensions must not be set for this provider: the vector width follows from embedder.model")
	}

	dims, known := modelDims[c.Model]
	if !known {
		return 0, fmt.Errorf("openai: embedder.model %s has no known vector width; supported models: %s",
			c.Model, strings.Join(slices.Sorted(maps.Keys(modelDims)), ", "))
	}
	return dims, nil
}

// unsupportedCapability names both sides, because the operator bound a role to
// this provider and the fix is one of the roles it does declare.
func unsupportedCapability(want lore.Capability, have lore.Capabilities) error {
	declared := have.Names()
	names := make([]string, len(declared))
	for i, name := range declared {
		names[i] = string(name)
	}
	return fmt.Errorf("openai: capability %s is not served by this provider; it serves %s", want, strings.Join(names, ", "))
}
