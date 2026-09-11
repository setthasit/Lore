package compat

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func TestEmbeddingsFollowEachPresetsPublishedEndpoint(t *testing.T) {
	for _, name := range presetKeys() {
		t.Run(name, func(t *testing.T) {
			_, err := Plugin().NewProvider(embedConfig(name, "some-embedding-model"))
			switch {
			case presets[name].embeddingsPath != "" && err != nil:
				t.Errorf("NewProvider for embed: %v", err)
			case presets[name].embeddingsPath == "" && err == nil:
				t.Error("NewProvider built an embedder for a preset with no embeddings endpoint")
			case presets[name].embeddingsPath == "" && !strings.Contains(err.Error(), name):
				t.Errorf("error %q does not name the preset", err)
			}
		})
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

// The preset documentation is where an operator reads these defaults, so it must offer every key in a stable order.
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
}

func TestEmbeddingsPathReachesAPresetWithoutEmbeddings(t *testing.T) {
	provider, err := Plugin().NewProvider(testConfig(lore.CapabilityEmbed, "embedding-3",
		`{"preset":"zai","embeddings_path":"/paas/v4/embeddings"}`))
	if err != nil {
		t.Fatalf("embeddings_path override did not reach embeddings: %v", err)
	}
	if _, ok := provider.(lore.Embedder); !ok {
		t.Fatalf("provider %T does not implement lore.Embedder", provider)
	}
}
