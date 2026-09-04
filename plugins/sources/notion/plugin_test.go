package notion

import (
	"encoding/json"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func TestManifestDeclaresWhatTheConnectorNeeds(t *testing.T) {
	m := Plugin().Manifest()

	if m.Name != "notion" || m.Kind != lore.KindSource || m.APIVersion != lore.APIVersion {
		t.Errorf("manifest identity = %q/%q/%d", m.Name, m.Kind, m.APIVersion)
	}
	if m.Capabilities != (lore.Capabilities{}) {
		t.Errorf("capabilities = %+v, want none: a page belongs to no repository", m.Capabilities)
	}

	if len(m.Fields) != 1 {
		t.Fatalf("fields = %+v, want root_pages only", m.Fields)
	}
	// An empty root_pages list syncs the whole shared workspace, so requiring the
	// key would make the connector's own default unreachable from configuration.
	if f := m.Fields[0]; f.Name != "root_pages" || f.Type != lore.FieldStringList || f.Required {
		t.Errorf("root_pages = %+v, want an optional string list", f)
	}

	if len(m.Secrets) != 1 {
		t.Fatalf("secrets = %+v, want the integration token only", m.Secrets)
	}
	if s := m.Secrets[0]; s.Key != "token" || s.ConfigField != "token_env" || s.DefaultEnv != "LORE_NOTION_TOKEN" {
		t.Errorf("token secret = %+v", s)
	}
}

func TestNewSourceBuildsTheConfiguredInstance(t *testing.T) {
	c, err := Plugin().NewSource(lore.SourceConfig{
		Instance: "notion-acme",
		Config:   json.RawMessage(`{"root_pages":["Engineering"]}`),
		Secrets:  map[string]string{"token": fakeToken},
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	got := c.(*Connector)
	if got.Name() != "notion-acme" {
		t.Errorf("Name() = %q, want the instance id", got.Name())
	}
	if len(got.rootPages) != 1 || got.rootPages[0] != "Engineering" {
		t.Errorf("rootPages = %v", got.rootPages)
	}
	if got.client.token != fakeToken {
		t.Error("the injected token did not reach the transport")
	}
	if got.client.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want the one real Notion endpoint", got.client.baseURL)
	}
}

func TestNewSourceRejectsAKeyTheManifestNeverDeclared(t *testing.T) {
	// A typo that decoded silently would sync a scope nobody asked for, and the
	// operator would have no way to tell from the output.
	_, err := Plugin().NewSource(lore.SourceConfig{
		Instance: "notion-acme",
		Config:   json.RawMessage(`{"root_page":["Engineering"]}`),
	})
	if err == nil {
		t.Fatal("NewSource accepted an undeclared configuration key")
	}
}
