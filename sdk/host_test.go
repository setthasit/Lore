package lore_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

type pluginSettings struct {
	URL     string `json:"url"`
	Retries int    `json:"retries"`
}

func TestSourceConfigDecode(t *testing.T) {
	tests := []struct {
		name     string
		instance string
		raw      json.RawMessage
		want     pluginSettings
		wantErr  []string
	}{
		{
			name:     "well-formed config",
			instance: "jira-acme",
			raw:      json.RawMessage(`{"url":"https://acme.atlassian.net","retries":3}`),
			want:     pluginSettings{URL: "https://acme.atlassian.net", Retries: 3},
		},
		{
			name:     "unknown key names the key and the instance",
			instance: "jira-acme",
			raw:      json.RawMessage(`{"url":"https://acme.atlassian.net","retires":3}`),
			wantErr:  []string{"jira-acme", `"retires"`},
		},
		{
			name:     "empty raw config",
			instance: "jira-acme",
			raw:      json.RawMessage(""),
		},
		{
			name:     "nil raw config",
			instance: "jira-acme",
			raw:      nil,
		},
		{
			name:     "malformed json",
			instance: "jira-acme",
			raw:      json.RawMessage(`{"url":`),
			wantErr:  []string{"jira-acme", "invalid configuration"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := lore.SourceConfig{Instance: tt.instance, Config: tt.raw}

			var got pluginSettings
			err := cfg.Decode(&got)

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Decode(%s) error = %v, want nil", tt.raw, err)
				}
				if got != tt.want {
					t.Errorf("Decode(%s) = %+v, want %+v", tt.raw, got, tt.want)
				}
				return
			}

			if err == nil {
				t.Fatalf("Decode(%s) = nil error, want error mentioning %q", tt.raw, tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Decode(%s) error = %q, want it to mention %s", tt.raw, err, want)
				}
			}
		})
	}
}

func TestProviderConfigDecode(t *testing.T) {
	t.Run("well-formed config", func(t *testing.T) {
		cfg := lore.ProviderConfig{
			Instance: "openai-main",
			Config:   json.RawMessage(`{"url":"https://api.openai.com","retries":2}`),
		}

		var got pluginSettings
		if err := cfg.Decode(&got); err != nil {
			t.Fatalf("Decode() error = %v, want nil", err)
		}

		want := pluginSettings{URL: "https://api.openai.com", Retries: 2}
		if got != want {
			t.Errorf("Decode() = %+v, want %+v", got, want)
		}
	})

	t.Run("unknown key names the key and the instance", func(t *testing.T) {
		cfg := lore.ProviderConfig{
			Instance: "openai-main",
			Config:   json.RawMessage(`{"base_url":"https://api.openai.com"}`),
		}

		err := cfg.Decode(&pluginSettings{})
		if err == nil {
			t.Fatal("Decode() = nil error, want error")
		}
		for _, want := range []string{"openai-main", `"base_url"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Decode() error = %q, want it to mention %s", err, want)
			}
		}
	})
}
