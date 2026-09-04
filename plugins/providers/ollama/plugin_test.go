package ollama

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func testProviderConfig(capability lore.Capability, model string) lore.ProviderConfig {
	return lore.ProviderConfig{
		Instance:   "ollama",
		Capability: capability,
		Model:      model,
		Config:     json.RawMessage(`{"base_url":"http://127.0.0.1:11500"}`),
	}
}

// Every capability the manifest declares must build a value satisfying the SDK
// interface the host will assert it against, or the manifest is a lie.
func TestPluginBuildsEveryDeclaredCapability(t *testing.T) {
	tests := []struct {
		capability lore.Capability
		model      string
		dimensions int
		check      func(t *testing.T, provider lore.Provider)
	}{
		{
			capability: lore.CapabilityEmbed,
			model:      testEmbedModel,
			dimensions: 768,
			check: func(t *testing.T, provider lore.Provider) {
				embedder, ok := provider.(lore.Embedder)
				if !ok {
					t.Fatalf("provider %T does not implement lore.Embedder", provider)
				}
				if got := embedder.Dimensions(); got != 768 {
					t.Errorf("Dimensions() = %d, want 768", got)
				}
			},
		},
		{
			capability: lore.CapabilityComplete,
			model:      "llama3.1",
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
			cfg := testProviderConfig(tt.capability, tt.model)
			cfg.Dimensions = tt.dimensions

			provider, err := Plugin().NewProvider(cfg)
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
	_, err := Plugin().NewProvider(testProviderConfig("rerank", testEmbedModel))
	if err == nil {
		t.Fatal("NewProvider succeeded for a capability the manifest does not declare")
	}

	const want = "ollama: capability rerank is not served by this provider; it serves embed, complete"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestPluginRequiresDimensionsForEmbed(t *testing.T) {
	_, err := Plugin().NewProvider(testProviderConfig(lore.CapabilityEmbed, testEmbedModel))
	if err == nil {
		t.Fatal("NewProvider accepted an embedder with no declared vector width")
	}

	const want = "ollama: embedder.dimensions must be set to the vector width of nomic-embed-text: " +
		"an Ollama model does not imply one; `ollama show nomic-embed-text` reports it"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// The daemon is unauthenticated, so a declared secret would make the host
// resolve, and an operator supply, a credential nothing ever sends.
func TestManifestDeclaresNoSecrets(t *testing.T) {
	if secrets := Plugin().Manifest().Secrets; len(secrets) != 0 {
		t.Errorf("Secrets = %v, want none for an unauthenticated daemon", secrets)
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
}
