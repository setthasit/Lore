package plugins

import (
	"testing"

	"github.com/setthasit/Lore/sdk"
)

// These are the checks the registry makes at registration; making them here too
// means a manifest that lies fails the build rather than a user's first sync.
func TestOfficialManifests(t *testing.T) {
	seen := make(map[string]bool)
	for _, p := range Official() {
		m := p.Manifest()

		t.Run(m.Name, func(t *testing.T) {
			if m.Name == "" {
				t.Fatalf("plugin %T has no name", p)
			}
			if seen[m.Name] {
				t.Fatalf("name %q is registered twice; two plugins cannot share a name", m.Name)
			}
			seen[m.Name] = true

			if m.APIVersion != lore.APIVersion {
				t.Errorf("APIVersion = %d, want %d", m.APIVersion, lore.APIVersion)
			}
			if m.Summary == "" {
				t.Error("Summary is empty; `lore plugin list` has nothing to show")
			}

			switch m.Kind {
			case lore.KindSource:
				if _, ok := p.(lore.SourcePlugin); !ok {
					t.Errorf("kind %s, but %T is no lore.SourcePlugin", m.Kind, p)
				}
			case lore.KindProvider:
				if _, ok := p.(lore.ProviderPlugin); !ok {
					t.Errorf("kind %s, but %T is no lore.ProviderPlugin", m.Kind, p)
				}
				if len(m.Capabilities.Names()) == 0 {
					t.Error("a provider that declares no capability can serve no binding")
				}
				for capability := range m.DefaultModels {
					if !m.Capabilities.Declares(capability) {
						t.Errorf("DefaultModels suggests a model for %s, a capability the manifest does not declare", capability)
					}
				}
			case lore.KindCode:
				if _, ok := p.(lore.CodePlugin); !ok {
					t.Errorf("kind %s, but %T is no lore.CodePlugin", m.Kind, p)
				}
			default:
				t.Errorf("Kind = %q, which is no kind the SDK defines", m.Kind)
			}

			for _, f := range m.Fields {
				if f.Name == "" {
					t.Error("a field with no name cannot appear in a `with:` block")
				}
			}
			// DefaultEnv is optional: a driver serving many vendors has no one
			// variable to suggest, so it makes the operator name theirs. Key and
			// ConfigField are not — without them nothing can be resolved at all.
			for _, s := range m.Secrets {
				if s.Key == "" || s.ConfigField == "" {
					t.Errorf("secret %+v is incomplete; the host cannot resolve it", s)
				}
			}
		})
	}
}
