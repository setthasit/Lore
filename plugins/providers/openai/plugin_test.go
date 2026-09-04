package openai

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func testConfig(capability lore.Capability, model string) lore.ProviderConfig {
	return lore.ProviderConfig{
		Instance:   "openai",
		Capability: capability,
		Model:      model,
		Config:     json.RawMessage(`{"base_url":"https://gateway.example"}`),
		Secrets:    map[string]string{"api_key": "test-key"},
	}
}

// Every capability the manifest declares must build a value satisfying the SDK
// interface the host will assert it against, or the manifest is a lie.
func TestPluginBuildsEveryDeclaredCapability(t *testing.T) {
	tests := []struct {
		capability lore.Capability
		model      string
		check      func(t *testing.T, provider lore.Provider)
	}{
		{
			capability: lore.CapabilityEmbed,
			model:      "text-embedding-3-large",
			check: func(t *testing.T, provider lore.Provider) {
				embedder, ok := provider.(lore.Embedder)
				if !ok {
					t.Fatalf("provider %T does not implement lore.Embedder", provider)
				}
				if got := embedder.Dimensions(); got != 3072 {
					t.Errorf("Dimensions() = %d, want 3072", got)
				}
			},
		},
		{
			capability: lore.CapabilityComplete,
			model:      "gpt-4o-mini",
			check: func(t *testing.T, provider lore.Provider) {
				if _, ok := provider.(lore.Completer); !ok {
					t.Fatalf("provider %T does not implement lore.Completer", provider)
				}
			},
		},
	}

	declared := Plugin().Manifest().Capabilities.Names()
	for _, tt := range tests {
		if !slices.Contains(declared, tt.capability) {
			t.Errorf("capability %s is tested but not declared", tt.capability)
		}

		t.Run(string(tt.capability), func(t *testing.T) {
			provider, err := Plugin().NewProvider(testConfig(tt.capability, tt.model))
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			tt.check(t, provider)
		})
	}

	if len(tests) != len(declared) {
		t.Errorf("declared capabilities %v, but %d are covered", declared, len(tests))
	}
}

func TestPluginRefusesUndeclaredCapability(t *testing.T) {
	_, err := Plugin().NewProvider(testConfig("rerank", "text-embedding-3-small"))
	if err == nil {
		t.Fatal("NewProvider succeeded for a capability the manifest does not declare")
	}

	const want = "openai: capability rerank is not served by this provider; it serves embed, complete"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestPluginRefusesDimensionsForEmbed(t *testing.T) {
	cfg := testConfig(lore.CapabilityEmbed, "text-embedding-3-small")
	cfg.Dimensions = 1536

	_, err := Plugin().NewProvider(cfg)
	if err == nil {
		t.Fatal("NewProvider accepted a declared vector width")
	}

	const want = "openai: embedder.dimensions must not be set for this provider: the vector width follows from embedder.model"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestPluginRefusesUnknownEmbeddingModel(t *testing.T) {
	_, err := Plugin().NewProvider(testConfig(lore.CapabilityEmbed, "text-embedding-9-huge"))
	if err == nil {
		t.Fatal("NewProvider accepted a model of unknown vector width")
	}

	const want = "openai: embedder.model text-embedding-9-huge has no known vector width; " +
		"supported models: text-embedding-3-large, text-embedding-3-small, text-embedding-ada-002"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestPluginRejectsUnknownConfigKey(t *testing.T) {
	cfg := testConfig(lore.CapabilityComplete, "gpt-4o-mini")
	cfg.Config = json.RawMessage(`{"bas_url":"https://gateway.example"}`)

	_, err := Plugin().NewProvider(cfg)
	if err == nil {
		t.Fatal("NewProvider accepted a key the manifest does not declare")
	}
	if !strings.Contains(err.Error(), "bas_url") {
		t.Errorf("error = %q, want it to name the unknown key", err)
	}
}

// `lore init` scaffolds a model from this map, so a missing suggestion writes an
// empty model into the file it tells the operator to use as-is.
func TestManifestSuggestsAModelPerCapability(t *testing.T) {
	m := Plugin().Manifest()
	for _, capability := range m.Capabilities.Names() {
		if m.DefaultModels[capability] == "" {
			t.Errorf("capability %s has no suggested model", capability)
		}
	}

	if _, known := modelDims[m.DefaultModels[lore.CapabilityEmbed]]; !known {
		t.Errorf("suggested embedding model %q has no known vector width, so the scaffold it generates is rejected",
			m.DefaultModels[lore.CapabilityEmbed])
	}
}
