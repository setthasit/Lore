package anthropic

import (
	"fmt"
	"strings"

	"github.com/setthasit/Lore/sdk"
)

// Plugin is the official Anthropic provider plugin.
func Plugin() lore.ProviderPlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:          "anthropic",
		Kind:          lore.KindProvider,
		APIVersion:    lore.APIVersion,
		Summary:       "Anthropic Claude chat completions",
		Capabilities:  lore.Capabilities{Complete: true},
		DefaultModels: map[lore.Capability]string{lore.CapabilityComplete: "claude-sonnet-4-5"},
		Fields: []lore.Field{
			{
				Name:    "base_url",
				Type:    lore.FieldURL,
				Default: DefaultBaseURL,
				Doc:     "override to reach a gateway or a proxy in front of the API",
				Prompt:  "Anthropic API base URL",
			},
		},
		Secrets: []lore.Secret{
			{
				Key:         "api_key",
				ConfigField: "api_key_env",
				DefaultEnv:  "ANTHROPIC_API_KEY",
				Doc:         "Anthropic API key, sent in the x-api-key header",
			},
		},
	}
}

// NewProvider serves completions only; the messages API embeds nothing, so an
// embed binding is a configuration mistake rather than a missing feature.
func (p plugin) NewProvider(c lore.ProviderConfig) (lore.Provider, error) {
	var cfg struct {
		BaseURL string `json:"base_url"`
	}
	if err := c.Decode(&cfg); err != nil {
		return nil, err
	}

	switch c.Capability {
	case lore.CapabilityComplete:
		return New(c.Secret("api_key"), c.Model, cfg.BaseURL)
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
	return fmt.Errorf("anthropic: capability %s is not served by this provider; it serves %s", want, strings.Join(names, ", "))
}
