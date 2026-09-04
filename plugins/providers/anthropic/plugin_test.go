package anthropic

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/setthasit/Lore/sdk"
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

// Every capability the manifest declares must build a value satisfying the SDK
// interface the host will assert it against, or the manifest is a lie.
func TestPluginBuildsEveryDeclaredCapability(t *testing.T) {
	tests := []struct {
		capability lore.Capability
		check      func(t *testing.T, provider lore.Provider)
	}{
		{
			capability: lore.CapabilityComplete,
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
			provider, err := Plugin().NewProvider(testProviderConfig(tt.capability))
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

// The messages API embeds nothing, so an embed binding must fail at
// construction rather than at the first query.
func TestPluginRefusesUndeclaredCapability(t *testing.T) {
	_, err := Plugin().NewProvider(testProviderConfig(lore.CapabilityEmbed))
	if err == nil {
		t.Fatal("NewProvider succeeded for a capability the manifest does not declare")
	}

	const want = "anthropic: capability embed is not served by this provider; it serves complete"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
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
