package ollama

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
)

func testProviderConfig(capability lore.Capability, model string) lore.ProviderConfig {
	return lore.ProviderConfig{
		Instance:   "ollama",
		Capability: capability,
		Model:      model,
		Config:     json.RawMessage(`{"base_url":"http://127.0.0.1:11500"}`),
	}
}

func TestPluginBuildsEveryDeclaredCapability(t *testing.T) {
	conform.Provider(t, Plugin(), func(capability lore.Capability) lore.ProviderConfig {
		cfg := testProviderConfig(capability, Plugin().Manifest().DefaultModels[capability])
		if capability == lore.CapabilityEmbed {
			cfg.Dimensions = 768
		}
		return cfg
	})
}

func TestPluginRequiresDimensionsForEmbed(t *testing.T) {
	_, err := Plugin().NewProvider(testProviderConfig(lore.CapabilityEmbed, testEmbedModel))
	if err == nil {
		t.Fatal("NewProvider accepted an embedder with no declared vector width")
	}
	msg := err.Error()
	if !strings.Contains(msg, "embedder.dimensions") || !strings.Contains(msg, testEmbedModel) {
		t.Errorf("error = %q, want it to name embedder.dimensions and the model", msg)
	}
}

// The daemon is unauthenticated, so a declared secret would have an operator supply a credential nothing sends.
func TestManifestDeclaresNoSecrets(t *testing.T) {
	if secrets := Plugin().Manifest().Secrets; len(secrets) != 0 {
		t.Errorf("Secrets = %v, want none for an unauthenticated daemon", secrets)
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
}
