package jira

import (
	"encoding/json"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func TestManifestDeclaresWhatTheConnectorNeeds(t *testing.T) {
	m := Plugin().Manifest()

	if m.Name != "jira" || m.Kind != lore.KindSource || m.APIVersion != lore.APIVersion {
		t.Errorf("manifest identity = %q/%q/%d", m.Name, m.Kind, m.APIVersion)
	}
	if m.Capabilities != (lore.Capabilities{}) {
		t.Errorf("capabilities = %+v, want none: an issue belongs to no repository", m.Capabilities)
	}

	fields := map[string]lore.Field{}
	for _, f := range m.Fields {
		fields[f.Name] = f
	}
	if len(fields) != 2 {
		t.Fatalf("fields = %+v, want base_url and projects", m.Fields)
	}
	if got := fields["base_url"]; got.Type != lore.FieldURL || !got.Required {
		t.Errorf("base_url = %+v, want a required url", got)
	}
	// An empty projects list is the whole-site sync, so requiring the key would
	// make the connector's own default unreachable from configuration.
	if got := fields["projects"]; got.Type != lore.FieldStringList || got.Required {
		t.Errorf("projects = %+v, want an optional string list", got)
	}

	secrets := map[string]lore.Secret{}
	for _, s := range m.Secrets {
		secrets[s.Key] = s
	}
	if got := secrets["email"]; got.ConfigField != "email_env" || got.DefaultEnv != "LORE_JIRA_EMAIL" {
		t.Errorf("email secret = %+v", got)
	}
	if got := secrets["token"]; got.ConfigField != "token_env" || got.DefaultEnv != "LORE_JIRA_TOKEN" {
		t.Errorf("token secret = %+v", got)
	}
	if len(secrets) != 2 {
		t.Errorf("secrets = %+v, want email and token only", m.Secrets)
	}
}

func TestNewSourceBuildsTheConfiguredInstance(t *testing.T) {
	c, err := Plugin().NewSource(lore.SourceConfig{
		Instance: "jira-acme",
		Config:   json.RawMessage(`{"base_url":"https://acme.atlassian.net/","projects":["PROJ"]}`),
		Secrets:  map[string]string{"email": fakeEmail, "token": fakeToken},
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	got := c.(*Connector)
	if got.Name() != "jira-acme" {
		t.Errorf("Name() = %q, want the instance id", got.Name())
	}
	if got.baseURL != "https://acme.atlassian.net" {
		t.Errorf("baseURL = %q", got.baseURL)
	}
	if len(got.projects) != 1 || got.projects[0] != "PROJ" {
		t.Errorf("projects = %v", got.projects)
	}
	if got.client.basicAuth != wantBasicAuth {
		t.Error("the injected credentials did not reach the transport")
	}
}

func TestNewSourceRejectsAKeyTheManifestNeverDeclared(t *testing.T) {
	// A typo that decoded silently would sync a scope nobody asked for, and the
	// operator would have no way to tell from the output.
	_, err := Plugin().NewSource(lore.SourceConfig{
		Instance: "jira-acme",
		Config:   json.RawMessage(`{"base_url":"https://acme.atlassian.net","project":["PROJ"]}`),
	})
	if err == nil {
		t.Fatal("NewSource accepted an undeclared configuration key")
	}
}
