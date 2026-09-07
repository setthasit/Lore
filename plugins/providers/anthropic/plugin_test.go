package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
)

func testProviderConfig(capability lore.Capability) lore.ProviderConfig {
	return lore.ProviderConfig{
		Instance:   "anthropic",
		Capability: capability,
		Model:      "claude-sonnet-4",
		Config:     json.RawMessage(`{"base_url":"https://gateway.example"}`),
		Secrets:    map[string]string{"api_key": "test-key"},
	}
}

func TestPluginBuildsEveryDeclaredCapability(t *testing.T) {
	conform.Provider(t, Plugin(), testProviderConfig)
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
