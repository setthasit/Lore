package compat

import (
	"maps"
	"slices"
	"strings"

	"github.com/setthasit/Lore/sdk"
)

// Vendors disagree on both host and path — Z.AI serves /paas/v4, DeepSeek omits /v1 — so a row carries each.
type preset struct {
	label          string
	baseURL        string
	chatPath       string
	embeddingsPath string

	defaultModels map[lore.Capability]string
}

// presets follows the provider table in docs/v3/08-extensibility.md.
var presets = map[string]preset{
	"zai": {
		label: "Z.AI (GLM)",
		// The China deployment serves the same paths under https://open.bigmodel.cn/api.
		baseURL:       "https://api.z.ai/api",
		chatPath:      "/paas/v4/chat/completions",
		defaultModels: map[lore.Capability]string{lore.CapabilityComplete: "glm-4.6"},
	},
	"openrouter": {
		label:          "OpenRouter",
		baseURL:        "https://openrouter.ai/api",
		chatPath:       "/v1/chat/completions",
		embeddingsPath: "/v1/embeddings",
		defaultModels: map[lore.Capability]string{
			lore.CapabilityComplete: "moonshotai/kimi-k2",
			lore.CapabilityEmbed:    "openai/text-embedding-3-small",
		},
	},
	"moonshot": {
		label:         "Moonshot (Kimi)",
		baseURL:       "https://api.moonshot.ai",
		chatPath:      "/v1/chat/completions",
		defaultModels: map[lore.Capability]string{lore.CapabilityComplete: "kimi-k3"},
	},
	"deepseek": {
		label:         "DeepSeek",
		baseURL:       "https://api.deepseek.com",
		chatPath:      "/chat/completions",
		defaultModels: map[lore.Capability]string{lore.CapabilityComplete: "deepseek-v4-pro"},
	},
	"groq": {
		label:         "Groq",
		baseURL:       "https://api.groq.com/openai",
		chatPath:      "/v1/chat/completions",
		defaultModels: map[lore.Capability]string{lore.CapabilityComplete: "openai/gpt-oss-120b"},
	},
	"together": {
		label:          "Together AI",
		baseURL:        "https://api.together.ai",
		chatPath:       "/v1/chat/completions",
		embeddingsPath: "/v1/embeddings",
		defaultModels: map[lore.Capability]string{
			lore.CapabilityComplete: "MiniMaxAI/MiniMax-M3",
			lore.CapabilityEmbed:    "intfloat/multilingual-e5-large-instruct",
		},
	},
	"vllm": {
		label:          "vLLM (self-hosted)",
		baseURL:        "http://localhost:8000",
		chatPath:       "/v1/chat/completions",
		embeddingsPath: "/v1/embeddings",
	},
	"lmstudio": {
		label:          "LM Studio (local)",
		baseURL:        "http://localhost:1234",
		chatPath:       "/v1/chat/completions",
		embeddingsPath: "/v1/embeddings",
	},
}

func presetKeys() []string {
	return slices.Sorted(maps.Keys(presets))
}

func presetDoc() string {
	rows := make([]string, 0, len(presets))
	for _, key := range presetKeys() {
		rows = append(rows, key+" — "+presets[key].summary())
	}
	return "OpenAI-compatible vendor to take the base URL, paths and default models from; " +
		"leave it unset and set base_url to reach a vendor this table does not list. Known presets: " +
		strings.Join(rows, "; ")
}

func (p preset) summary() string {
	parts := []string{p.label}
	if model := p.defaultModels[lore.CapabilityComplete]; model != "" {
		parts = append(parts, "complete "+model)
	}
	switch {
	case p.embeddingsPath == "":
		parts = append(parts, "no embeddings endpoint")
	case p.defaultModels[lore.CapabilityEmbed] != "":
		parts = append(parts, "embed "+p.defaultModels[lore.CapabilityEmbed])
	}
	if len(p.defaultModels) == 0 {
		parts = append(parts, "name the models you serve")
	}
	return strings.Join(parts, ", ")
}
