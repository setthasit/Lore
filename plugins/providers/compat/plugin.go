package compat

import (
	"fmt"
	"strings"

	"github.com/setthasit/Lore/sdk"
)

const pluginName = "openai-compatible"

// Plugin is the official driver for vendors serving the OpenAI protocols.
func Plugin() lore.ProviderPlugin { return plugin{} }

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       pluginName,
		Kind:       lore.KindProvider,
		APIVersion: lore.APIVersion,
		Summary:    "Embeddings and chat completions from any vendor speaking the OpenAI protocols",
		// Both halves are declared because the driver as a whole serves both.
		// A preset whose vendor publishes no embeddings endpoint refuses at
		// construction instead, where the error can name that preset.
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
				Name:    "chat_path",
				Type:    lore.FieldString,
				Default: defaultChatPath,
				Doc:     "overrides the preset's chat completions path, for a vendor that serves the protocol somewhere else",
			},
			{
				Name:    "embeddings_path",
				Type:    lore.FieldString,
				Default: defaultEmbeddingsPath,
				Doc:     "overrides the preset's embeddings path, and reaches embeddings on a preset whose row declares none",
			},
		},
		Secrets: []lore.Secret{
			{
				Key:         "api_key",
				ConfigField: "api_key_env",
				// No DefaultEnv: every vendor names its own variable, so a
				// suggestion here would be wrong for eight of the nine.
				Doc: "vendor API key, sent as a bearer token; a local server that authenticates nothing needs none",
			},
		},
		// DefaultModels stays nil. It is documentation the host shows in
		// scaffolds and prompts, never applied, and one driver over nine
		// vendors has no single model to suggest — each row's default is in the
		// preset field's Doc, where the operator reads it while choosing one.
	}
}

// NewProvider builds only the half it was asked for: an embedding model and a
// chat model are separate connections configured from different bindings, so
// building both would demand configuration for a role nobody asked this
// instance to play.
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
	return fmt.Errorf("%s: capability %s is not served by this provider; it serves %s", pluginName, want, strings.Join(names, ", "))
}
