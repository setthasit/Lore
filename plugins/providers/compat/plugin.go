package compat

import (
	"fmt"

	"github.com/setthasit/Lore/sdk"
)

const pluginName = "openai-compatible"

func Plugin() lore.ProviderPlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:         pluginName,
		Kind:         lore.KindProvider,
		APIVersion:   lore.APIVersion,
		Summary:      "Embeddings and chat completions from any vendor speaking the OpenAI protocols",
		Capabilities: lore.Capabilities{Embed: true, Complete: true},
		Fields: []lore.Field{
			{
				Name:   "preset",
				Type:   lore.FieldString,
				Doc:    presetDoc(),
				Prompt: "Vendor preset, or blank to configure a base URL yourself",
			},
			{
				Name:   "base_url",
				Type:   lore.FieldURL,
				Doc:    "overrides the preset's base URL; required instead of preset when the vendor is not in the preset table",
				Prompt: "OpenAI-compatible base URL",
			},
			{
				Name: "chat_path",
				Type: lore.FieldString,
				Doc:  "overrides the preset's chat completions path, for a vendor that serves the protocol somewhere else",
			},
			{
				Name: "embeddings_path",
				Type: lore.FieldString,
				Doc:  "overrides the preset's embeddings path, and reaches embeddings on a preset whose row declares none",
			},
		},
		Secrets: []lore.Secret{
			{
				Key:         "api_key",
				ConfigField: "api_key_env",
				Doc:         "vendor API key, sent as a bearer token; a local server that authenticates nothing needs none",
			},
		},
	}
}

func (p plugin) NewProvider(c lore.ProviderConfig) (lore.Provider, error) {
	var cfg config
	if err := c.Decode(&cfg); err != nil {
		return nil, err
	}

	d, err := resolve(cfg)
	if err != nil {
		return nil, err
	}

	switch c.Capability {
	case lore.CapabilityEmbed:
		return d.newEmbedder(c)
	case lore.CapabilityComplete:
		return d.newCompleter(c)
	default:
		return nil, fmt.Errorf("%s: capability %s is not served by this provider; it serves %s", pluginName, c.Capability, p.Manifest().Capabilities)
	}
}
