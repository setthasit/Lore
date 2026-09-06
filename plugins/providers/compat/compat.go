// Package compat reaches any vendor speaking the OpenAI chat completions and
// embeddings protocols, driving them through the OpenAI implementation of those
// protocols and differing only in the endpoint each vendor publishes.
package compat

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/setthasit/Lore/plugins/providers/openai"
	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/httpx"
)

// The protocol's own paths answer for a vendor absent from the table: an
// OpenAI-compatible API that moved them says so through chat_path and
// embeddings_path, which is the whole reason those fields exist.
const (
	defaultChatPath       = "/v1/chat/completions"
	defaultEmbeddingsPath = "/v1/embeddings"
)

// config is the `with:` block. The key naming the environment variable is
// absent by design: the host resolves it and injects the value.
type config struct {
	Preset         string `json:"preset"`
	BaseURL        string `json:"base_url"`
	ChatPath       string `json:"chat_path"`
	EmbeddingsPath string `json:"embeddings_path"`
}

// driver is a preset row with the operator's overrides already applied, so the
// two construction paths below read a resolved endpoint instead of deciding
// precedence twice and disagreeing.
type driver struct {
	// name prefixes every error the underlying client raises. It carries the
	// preset because a workspace may hold several instances of this one plugin,
	// and "openai-compatible" alone would not say which vendor failed.
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
		// Both keys are named because either one alone is a complete answer:
		// a listed vendor is a preset, an unlisted one is a base URL.
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
