package compat

import (
	"maps"
	"slices"
	"strings"

	"github.com/setthasit/Lore/sdk"
)

// embeddingsUnsupported marks a vendor that publishes no OpenAI-compatible
// embeddings endpoint. It is a marker rather than a plausible-looking path
// because a guessed path is only discovered when the first index write answers
// 404, and by then the operator has already committed a vector space.
const embeddingsUnsupported = ""

// preset is one vendor's published OpenAI-compatible surface. The base URL and
// the two paths are kept apart because vendors disagree about both halves —
// Z.AI serves /paas/v4, Groq puts /openai in front of /v1, DeepSeek omits /v1
// entirely — so an operator moving one vendor behind a gateway overrides the
// host without retyping that vendor's path layout.
type preset struct {
	label          string
	baseURL        string
	chatPath       string
	embeddingsPath string

	// defaultModels suggests a model per capability this row serves. A
	// self-hosted row leaves it empty: the model is whichever one the operator
	// loaded, and no table can know that.
	defaultModels map[lore.Capability]string
}

// presets is the vendor table. Rows follow the provider table in
// docs/v3/08-extensibility.md so the two can be read against each other, and
// every URL and path below is the one that vendor's own documentation
// publishes rather than the OpenAI shape assumed on its behalf.
var presets = map[string]preset{
	"zai": {
		label: "Z.AI (GLM)",
		// The China deployment serves the same paths under
		// https://open.bigmodel.cn/api, which is a base_url override.
		baseURL:        "https://api.z.ai/api",
		chatPath:       "/paas/v4/chat/completions",
		embeddingsPath: embeddingsUnsupported,
		defaultModels:  map[lore.Capability]string{lore.CapabilityComplete: "glm-4.6"},
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
		label:          "Moonshot (Kimi)",
		baseURL:        "https://api.moonshot.ai",
		chatPath:       "/v1/chat/completions",
		embeddingsPath: embeddingsUnsupported,
		defaultModels:  map[lore.Capability]string{lore.CapabilityComplete: "kimi-k3"},
	},
	"deepseek": {
		label:          "DeepSeek",
		baseURL:        "https://api.deepseek.com",
		chatPath:       "/chat/completions",
		embeddingsPath: embeddingsUnsupported,
		defaultModels:  map[lore.Capability]string{lore.CapabilityComplete: "deepseek-v4-pro"},
	},
	"groq": {
		label:          "Groq",
		baseURL:        "https://api.groq.com/openai",
		chatPath:       "/v1/chat/completions",
		embeddingsPath: embeddingsUnsupported,
		defaultModels:  map[lore.Capability]string{lore.CapabilityComplete: "openai/gpt-oss-120b"},
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
	// Both self-hosted rows carry the port their project listens on out of the
	// box, so an operator running one locally names the preset and nothing
	// else, and anyone serving it elsewhere overrides base_url.
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

// presetKeys lists the keys an operator may write, sorted, because they appear
// in a manifest field's documentation and in every error that offers them: a
// map's order would churn generated scaffolds between builds.
func presetKeys() []string {
	return slices.Sorted(maps.Keys(presets))
}

// presetDoc is what the operator reads while choosing a preset, which is why
// each row's default models are spelled out here rather than in the manifest's
// DefaultModels: one static map cannot suggest a model per vendor.
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
	case p.embeddingsPath == embeddingsUnsupported:
		parts = append(parts, "no embeddings endpoint")
	case p.defaultModels[lore.CapabilityEmbed] != "":
		parts = append(parts, "embed "+p.defaultModels[lore.CapabilityEmbed])
	}
	if len(p.defaultModels) == 0 {
		parts = append(parts, "name the models you serve")
	}
	return strings.Join(parts, ", ")
}
