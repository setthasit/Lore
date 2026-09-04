package compat

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

// The expectations below are full URLs on purpose: a preset is a promise about
// where a vendor is reached, and a table built from the same constants the code
// uses would assert nothing about that promise.
func TestPresetsResolveDocumentedEndpoints(t *testing.T) {
	cases := []struct {
		preset        string
		chat          string
		embeddings    string // empty means the row declares embeddings unsupported
		completeModel string
		embedModel    string
	}{
		{
			preset:        "zai",
			chat:          "https://api.z.ai/api/paas/v4/chat/completions",
			completeModel: "glm-4.6",
		},
		{
			preset:        "openrouter",
			chat:          "https://openrouter.ai/api/v1/chat/completions",
			embeddings:    "https://openrouter.ai/api/v1/embeddings",
			completeModel: "moonshotai/kimi-k2",
			embedModel:    "openai/text-embedding-3-small",
		},
		{
			preset:        "moonshot",
			chat:          "https://api.moonshot.ai/v1/chat/completions",
			completeModel: "kimi-k3",
		},
		{
			preset:        "deepseek",
			chat:          "https://api.deepseek.com/chat/completions",
			completeModel: "deepseek-v4-pro",
		},
		{
			preset:        "groq",
			chat:          "https://api.groq.com/openai/v1/chat/completions",
			completeModel: "openai/gpt-oss-120b",
		},
		{
			preset:        "together",
			chat:          "https://api.together.ai/v1/chat/completions",
			embeddings:    "https://api.together.ai/v1/embeddings",
			completeModel: "MiniMaxAI/MiniMax-M3",
			embedModel:    "intfloat/multilingual-e5-large-instruct",
		},
		{
			preset:     "vllm",
			chat:       "http://localhost:8000/v1/chat/completions",
			embeddings: "http://localhost:8000/v1/embeddings",
		},
		{
			preset:     "lmstudio",
			chat:       "http://localhost:1234/v1/chat/completions",
			embeddings: "http://localhost:1234/v1/embeddings",
		},
	}

	for _, tc := range cases {
		t.Run(tc.preset, func(t *testing.T) {
			d, err := resolve(config{Preset: tc.preset})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if d.chatEndpoint != tc.chat {
				t.Errorf("chat endpoint = %q, want %q", d.chatEndpoint, tc.chat)
			}
			if d.embeddingsEndpoint != tc.embeddings {
				t.Errorf("embeddings endpoint = %q, want %q", d.embeddingsEndpoint, tc.embeddings)
			}
			if got := d.defaultModels[lore.CapabilityComplete]; got != tc.completeModel {
				t.Errorf("default complete model = %q, want %q", got, tc.completeModel)
			}
			if got := d.defaultModels[lore.CapabilityEmbed]; got != tc.embedModel {
				t.Errorf("default embed model = %q, want %q", got, tc.embedModel)
			}

			// The row's verdict on embeddings is what the operator meets: a
			// path resolves, or construction refuses and names the preset.
			_, err = Plugin().NewProvider(embedConfig(tc.preset, "some-embedding-model"))
			switch {
			case tc.embeddings != "" && err != nil:
				t.Errorf("NewProvider for embed: %v", err)
			case tc.embeddings == "" && err == nil:
				t.Error("NewProvider built an embedder for a preset with no embeddings endpoint")
			case tc.embeddings == "" && !strings.Contains(err.Error(), tc.preset):
				t.Errorf("error %q does not name the preset", err)
			}
		})
	}

	if len(cases) != len(presets) {
		t.Errorf("%d presets are covered, but the table has %d", len(cases), len(presets))
	}
}

func embedConfig(preset, model string) lore.ProviderConfig {
	return lore.ProviderConfig{
		Instance:   "compat",
		Capability: lore.CapabilityEmbed,
		Model:      model,
		Dimensions: 1024,
		Config:     json.RawMessage(`{"preset":"` + preset + `"}`),
		Secrets:    map[string]string{"api_key": fakeKey},
	}
}

// The preset documentation is the only place an operator reads a row's default
// models, so it must offer every key, in a stable order.
func TestPresetDocOffersEveryPresetSorted(t *testing.T) {
	doc := presetDoc()

	at := make([]int, 0, len(presets))
	for _, key := range presetKeys() {
		i := strings.Index(doc, key+" — ")
		if i < 0 {
			t.Fatalf("preset %s is missing from the field documentation", key)
		}
		at = append(at, i)
	}
	if !slices.IsSorted(at) {
		t.Errorf("presets are documented out of order: %v", at)
	}

	for _, want := range []string{"glm-4.6", "intfloat/multilingual-e5-large-instruct", "no embeddings endpoint"} {
		if !strings.Contains(doc, want) {
			t.Errorf("field documentation omits %q", want)
		}
	}
}
