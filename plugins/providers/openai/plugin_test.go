package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
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

func TestPluginBuildsEveryDeclaredCapability(t *testing.T) {
	conform.Provider(t, Plugin(), func(capability lore.Capability) lore.ProviderConfig {
		return testConfig(capability, Plugin().Manifest().DefaultModels[capability])
	})
}

func TestEmbedderWidthFollowsTheModel(t *testing.T) {
	provider, err := Plugin().NewProvider(testConfig(lore.CapabilityEmbed, "text-embedding-3-large"))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if got := provider.(lore.Embedder).Dimensions(); got != 3072 {
		t.Errorf("Dimensions() = %d, want 3072", got)
	}
}

func TestPluginRefusesDimensionsForEmbed(t *testing.T) {
	cfg := testConfig(lore.CapabilityEmbed, "text-embedding-3-small")
	cfg.Dimensions = 1536

	_, err := Plugin().NewProvider(cfg)
	if err == nil {
		t.Fatal("NewProvider accepted a declared vector width")
	}

	msg := err.Error()
	if !strings.Contains(msg, "embedder.dimensions") || !strings.Contains(msg, "must not be set") ||
		!strings.Contains(msg, "embedder.model") {
		t.Errorf("error = %q, want it to refuse embedder.dimensions and point at embedder.model", msg)
	}
}

func TestPluginRefusesUnknownEmbeddingModel(t *testing.T) {
	_, err := Plugin().NewProvider(testConfig(lore.CapabilityEmbed, "text-embedding-9-huge"))
	if err == nil {
		t.Fatal("NewProvider accepted a model of unknown vector width")
	}
	msg := err.Error()
	if !strings.Contains(msg, "text-embedding-9-huge") || !strings.Contains(msg, "no known vector width") {
		t.Errorf("error = %q, want it to name the model and its unknown width", msg)
	}
}

// `lore init` scaffolds a model from this map, so a missing suggestion writes an empty model.
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
