package conform

import (
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func Provider(t *testing.T, p lore.ProviderPlugin, configFor func(lore.Capability) lore.ProviderConfig) {
	t.Helper()

	declared := p.Manifest().Capabilities.Names()
	if len(declared) == 0 {
		t.Fatal("manifest declares no capabilities: every assertion below would hold vacuously")
	}

	for _, capability := range declared {
		t.Run(string(capability), func(t *testing.T) {
			cfg := configFor(capability)
			provider, err := p.NewProvider(cfg)
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			assertCapability(t, capability, provider, cfg)
		})
	}
}

func assertCapability(t *testing.T, capability lore.Capability, provider lore.Provider, cfg lore.ProviderConfig) {
	t.Helper()

	switch capability {
	case lore.CapabilityEmbed:
		embedder, ok := provider.(lore.Embedder)
		if !ok {
			t.Fatalf("provider %T does not implement lore.Embedder", provider)
		}
		got := embedder.Dimensions()
		if got <= 0 {
			t.Errorf("Dimensions() = %d, want a positive vector width", got)
		}
		if want := cfg.Dimensions; want != 0 && got != want {
			t.Errorf("Dimensions() = %d, want the declared %d", got, want)
		}
	case lore.CapabilityComplete:
		if _, ok := provider.(lore.Completer); !ok {
			t.Fatalf("provider %T does not implement lore.Completer", provider)
		}
	default:
		t.Fatalf("capability %s is declared but this suite asserts no shape for it", capability)
	}
}
