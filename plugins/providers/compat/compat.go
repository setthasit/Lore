// Package compat drives any vendor speaking the OpenAI chat completions and embeddings protocols.
package compat

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/setthasit/Lore/plugins/providers/openai"
	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/httpx"
)

const (
	defaultChatPath       = "/v1/chat/completions"
	defaultEmbeddingsPath = "/v1/embeddings"
)

type config struct {
	Preset         string `json:"preset"`
	BaseURL        string `json:"base_url"`
	ChatPath       string `json:"chat_path"`
	EmbeddingsPath string `json:"embeddings_path"`
}

type driver struct {
	name   string
	preset string

	chatEndpoint       string
	embeddingsEndpoint string // empty when the preset serves no embeddings
}

func resolve(cfg config) (driver, error) {
	row, known := presets[cfg.Preset]
	switch {
	case cfg.Preset != "" && !known:
		return driver{}, fmt.Errorf("%s: preset %s is not one this build knows; known presets: %s",
			pluginName, cfg.Preset, strings.Join(presetKeys(), ", "))
	case cfg.Preset == "" && cfg.BaseURL == "":
		return driver{}, fmt.Errorf("%s: set preset to one of %s, or set base_url to the vendor's OpenAI-compatible base URL",
			pluginName, strings.Join(presetKeys(), ", "))
	}

	if cfg.Preset == "" {
		row = preset{chatPath: defaultChatPath, embeddingsPath: defaultEmbeddingsPath}
	}

	d := driver{
		name:         pluginName,
		preset:       cfg.Preset,
		chatEndpoint: httpx.Endpoint(cfg.BaseURL, row.baseURL, cmp.Or(cfg.ChatPath, row.chatPath)),
	}
	if cfg.Preset != "" {
		d.name += "/" + cfg.Preset
	}
	if path := cmp.Or(cfg.EmbeddingsPath, row.embeddingsPath); path != "" {
		d.embeddingsEndpoint = httpx.Endpoint(cfg.BaseURL, row.baseURL, path)
	}
	return d, nil
}

func (d driver) newCompleter(c lore.ProviderConfig) (lore.Provider, error) {
	return openai.NewCompatible(d.name, c.Secret("api_key"), c.Model, d.chatEndpoint)
}

func (d driver) newEmbedder(c lore.ProviderConfig) (lore.Provider, error) {
	if d.embeddingsEndpoint == "" {
		return nil, fmt.Errorf("%s: preset %s serves no OpenAI-compatible embeddings endpoint; bind embedder to a provider that does, or set embeddings_path if this vendor has since published one",
			pluginName, d.preset)
	}
	if c.Dimensions <= 0 {
		return nil, fmt.Errorf("%s: embedder.dimensions must be set to the vector width of %s: this driver serves any vendor, so its models imply no width",
			pluginName, c.Model)
	}
	return openai.NewEmbedderAt(d.name, c.Secret("api_key"), c.Model, d.embeddingsEndpoint, c.Dimensions)
}
