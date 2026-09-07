package registry

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

func providerManifest(name string, caps lore.Capabilities) lore.Manifest {
	return lore.Manifest{
		Name:         name,
		Kind:         lore.KindProvider,
		APIVersion:   lore.APIVersion,
		Summary:      "a provider that exists only in this test",
		Capabilities: caps,
		Fields:       []lore.Field{{Name: "base_url", Type: lore.FieldURL, Default: "https://api.acme.dev"}},
		Secrets:      []lore.Secret{{Key: "api_key", ConfigField: "api_key_env", DefaultEnv: "ACME_API_KEY"}},
	}
}

func honest(caps lore.Capabilities) stubProvider {
	return stubProvider{
		manifest: providerManifest("acme", caps),
		build: func(c lore.ProviderConfig) (lore.Provider, error) {
			switch c.Capability {
			case lore.CapabilityEmbed:
				return embedOnly{dims: 1536}, nil
			default:
				return completeOnly{}, nil
			}
		},
	}
}

func TestBuildProviderRefusesARoleTheProviderDoesNotServe(t *testing.T) {
	t.Setenv("ACME_API_KEY", "sk-example")

	r := newRegistry(t, honest(lore.Capabilities{Embed: true}))

	_, err := r.BuildProvider(Binding{
		Provider:   "acme",
		Model:      "acme-chat",
		Capability: lore.CapabilityComplete,
		Field:      "llm",
	}, nil)
	if err == nil {
		t.Fatal("BuildProvider: want an error")
	}
	for _, want := range []string{`llm binds provider "acme"`, "does not serve complete", "it serves embed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestBuildProviderRejectsACapabilityLie(t *testing.T) {
	t.Setenv("ACME_API_KEY", "sk-example")

	r := newRegistry(t, stubProvider{
		manifest: providerManifest("acme", lore.Capabilities{Embed: true, Complete: true}),
		build: func(lore.ProviderConfig) (lore.Provider, error) {
			return completeOnly{}, nil
		},
	})

	_, err := r.BuildProvider(Binding{
		Provider:   "acme",
		Model:      "acme-embed",
		Capability: lore.CapabilityEmbed,
		Field:      "embedder",
	}, nil)
	if err == nil {
		t.Fatal("BuildProvider: want an error")
	}
	if !strings.Contains(err.Error(), "built a value that does not implement it") {
		t.Errorf("error %q does not name the broken claim", err)
	}
}

func TestBuildProviderBuildsAnImplicitInstanceFromPluginDefaults(t *testing.T) {
	t.Setenv("ACME_API_KEY", "sk-example")

	var got lore.ProviderConfig
	r := newRegistry(t, stubProvider{
		manifest: providerManifest("acme", lore.Capabilities{Embed: true}),
		build: func(c lore.ProviderConfig) (lore.Provider, error) {
			got = c
			return embedOnly{dims: 768}, nil
		},
	})

	built, err := r.BuildProvider(Binding{
		Provider:   "acme",
		Model:      "acme-embed",
		Dimensions: 768,
		Capability: lore.CapabilityEmbed,
		Field:      "embedder",
	}, nil)
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if _, ok := built.Value.(lore.Embedder); !ok {
		t.Fatalf("built %T, want a lore.Embedder", built.Value)
	}
	if string(got.Config) != "{}" {
		t.Errorf("config = %s, want an empty block for an implicit instance", got.Config)
	}
}

func TestBuildProviderPrefersADeclaredInstanceOverThePluginDefaults(t *testing.T) {
	t.Setenv("LORE_OPENROUTER_KEY", "sk-example")

	var got lore.ProviderConfig
	r := newRegistry(t, stubProvider{
		manifest: providerManifest("acme", lore.Capabilities{Complete: true}),
		build: func(c lore.ProviderConfig) (lore.Provider, error) {
			got = c
			return completeOnly{}, nil
		},
	})

	instances := []Instance{{
		ID:  "openrouter",
		Use: "acme",
		With: map[string]any{
			"base_url":    "https://openrouter.ai/api",
			"api_key_env": "LORE_OPENROUTER_KEY",
		},
		Field: "providers[openrouter]",
	}}

	if _, err := r.BuildProvider(Binding{
		Provider:   "openrouter",
		Model:      "moonshotai/kimi-k2",
		Capability: lore.CapabilityComplete,
		Field:      "llm",
	}, instances); err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}

	if got.Secret("api_key") != "sk-example" {
		t.Errorf("api_key = %q, want the value of the named variable", got.Secret("api_key"))
	}

	var decoded map[string]any
	if err := json.Unmarshal(got.Config, &decoded); err != nil {
		t.Fatalf("decode the delivered config: %v", err)
	}
	if _, leaked := decoded["api_key_env"]; leaked {
		t.Errorf("config %s carries the operator's variable name", got.Config)
	}
	if decoded["base_url"] != "https://openrouter.ai/api" {
		t.Errorf("config %s does not carry base_url", got.Config)
	}
}

func TestPrepareRejectsBrokenWithBlocks(t *testing.T) {
	manifest := lore.Manifest{
		Name:       "acme",
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "a source that exists only in this test",
		Fields: []lore.Field{
			{Name: "base_url", Type: lore.FieldURL, Required: true, Default: "https://acme.dev"},
			{Name: "projects", Type: lore.FieldStringList},
			{Name: "page_size", Type: lore.FieldInt},
			{Name: "verbose", Type: lore.FieldBool},
			{Name: "window", Type: lore.FieldDuration},
		},
		Secrets: []lore.Secret{{Key: "token", ConfigField: "token_env", DefaultEnv: "ACME_TOKEN"}},
	}

	tests := []struct {
		name string
		with map[string]any
		want []string
	}{
		{
			name: "unknown key",
			with: map[string]any{"base_url": "https://acme.dev", "porjects": []any{"P"}},
			want: []string{"sources[acme].with.porjects", "is not a key", "projects"},
		},
		{
			name: "missing required field",
			with: map[string]any{"projects": []any{"P"}},
			want: []string{"sources[acme].with.base_url", "must be set"},
		},
		{
			name: "url that no request can be built from",
			with: map[string]any{"base_url": "acme.dev"},
			want: []string{"sources[acme].with.base_url", "must be an absolute http(s) URL"},
		},
		{
			name: "list holding something that is not a string",
			with: map[string]any{"base_url": "https://acme.dev", "projects": []any{"P", 7}},
			want: []string{"sources[acme].with.projects[1]", "must be a string"},
		},
		{
			name: "scalar where a list is declared",
			with: map[string]any{"base_url": "https://acme.dev", "projects": "P"},
			want: []string{"sources[acme].with.projects", "must be a list of strings"},
		},
		{
			name: "fractional value for a whole number",
			with: map[string]any{"base_url": "https://acme.dev", "page_size": 1.5},
			want: []string{"sources[acme].with.page_size", "must be a whole number"},
		},
		{
			name: "string where a boolean is declared",
			with: map[string]any{"base_url": "https://acme.dev", "verbose": "yes"},
			want: []string{"sources[acme].with.verbose", "must be true or false"},
		},
		{
			name: "unparseable duration",
			with: map[string]any{"base_url": "https://acme.dev", "window": "a fortnight"},
			want: []string{"sources[acme].with.window", "is not a duration"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ACME_TOKEN", "t-example")

			r := newRegistry(t, stubSource{manifest: manifest})
			_, err := r.BuildSources([]Instance{{Use: "acme", With: tt.with, Field: "sources[acme]"}})
			if err == nil {
				t.Fatal("BuildSources: want an error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestPrepareReportsTheSameFirstBadKeyOnEveryRun(t *testing.T) {
	r := newRegistry(t, stubSource{manifest: sourceManifest("acme")})

	const want = `sources[acme].with.alpha is not a key plugin "acme" accepts`
	for range 32 {
		_, err := r.BuildSources([]Instance{{Use: "acme", Field: "sources[acme]", With: map[string]any{
			"zeta":  1,
			"mid":   1,
			"alpha": 1,
		}}})
		if err == nil {
			t.Fatal("BuildSources: want an error")
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain http is as absolute as https",
			raw:  "http://acme.dev",
		},
		{
			name: "a scheme without a host names no server",
			raw:  "http:///v1",
			want: "base_url must be an absolute http(s) URL like https://acme.dev, got http:///v1",
		},
		{
			name: "a host no parser accepts",
			raw:  "http:// acme.dev",
			want: "base_url is not a URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckURL("base_url", tt.raw, "https://acme.dev")
			if tt.want == "" {
				if err != nil {
					t.Fatalf("CheckURL(%q): %v", tt.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckURL(%q): want an error", tt.raw)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			if !internalerror.IsBadRequest(err) {
				t.Errorf("error %q is not a bad request", err)
			}
		})
	}
}

func TestCheckURLRefusalDoesNotEchoURLCredentials(t *testing.T) {
	const (
		user  = "svcaccount"
		token = "fake-not-a-real-token"
		query = "sig=fake-signature"
	)

	cases := map[string]string{
		"a scheme that is not http(s)": "ftp://" + user + ":" + token + "@acme.dev/v1?" + query,
		"a host no parser accepts":     "http://" + user + ":" + token + "@ acme.dev/v1?" + query,
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			err := CheckURL("base_url", raw, "https://acme.dev")
			if err == nil {
				t.Fatalf("CheckURL(%q): want a refusal", raw)
			}

			message := internalerror.MessageOf(err)
			for _, secret := range []string{user, token, query} {
				if strings.Contains(message, secret) {
					t.Errorf("refusal %q echoes %q", message, secret)
				}
			}
			if !strings.Contains(message, "base_url") {
				t.Errorf("refusal %q does not name the field", message)
			}
		})
	}
}

func TestPrepareRejectsAnUnsetSecretVariable(t *testing.T) {
	manifest := sourceManifest("acme")
	manifest.Secrets = []lore.Secret{{Key: "token", ConfigField: "token_env", DefaultEnv: "ACME_TOKEN"}}

	r := newRegistry(t, stubSource{manifest: manifest})

	t.Run("named explicitly", func(t *testing.T) {
		t.Setenv("LORE_ACME_TOKEN", "")

		_, err := r.BuildSources([]Instance{{
			Use:   "acme",
			With:  map[string]any{"token_env": "LORE_ACME_TOKEN"},
			Field: "sources[acme]",
		}})
		if err == nil {
			t.Fatal("BuildSources: want an error")
		}
		want := "sources[acme].with.token_env names LORE_ACME_TOKEN, but that environment variable is not set"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	})

	t.Run("left to the manifest default", func(t *testing.T) {
		t.Setenv("ACME_TOKEN", "")

		_, err := r.BuildSources([]Instance{{Use: "acme", Field: "sources[acme]"}})
		if err == nil {
			t.Fatal("BuildSources: want an error")
		}
		if !strings.Contains(err.Error(), "names ACME_TOKEN, but that environment variable is not set") {
			t.Errorf("error %q does not fall back to the manifest's variable", err)
		}
	})
}

func TestBuildCodeBindsEachCloneToItsRoot(t *testing.T) {
	var roots []string
	r := newRegistry(t, codePlugin{
		manifest: codeManifest("git"),
		build: func(c lore.CodeConfig) (lore.CodeRepo, error) {
			roots = append(roots, c.Root)
			return stubRepo{}, nil
		},
	})

	built, err := r.BuildCode([]LocalClone{
		{Path: "/w/app", Use: "git", Remote: "github:acme/app", Field: "repos[0]"},
		{Path: "/w/infra", Use: "git", Field: "repos[1]"},
	})
	if err != nil {
		t.Fatalf("BuildCode: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("built %d accessors, want one per clone", len(built))
	}
	if len(roots) != 2 || roots[0] != "/w/app" || roots[1] != "/w/infra" {
		t.Errorf("roots = %v, want each clone's own path", roots)
	}
}

func TestBuildLendsAPluginALoggerEvenWhenTheHostCarriesNone(t *testing.T) {
	logged := false
	r := newRegistry(t, codePlugin{
		manifest: codeManifest("git"),
		build: func(c lore.CodeConfig) (lore.CodeRepo, error) {
			c.Host.Log.Info("opening a clone")
			logged = true
			return stubRepo{}, nil
		},
	})

	if _, err := r.BuildCode([]LocalClone{{Path: "/w/app", Use: "git", Field: "repos[0]"}}); err != nil {
		t.Fatalf("BuildCode: %v", err)
	}
	if !logged {
		t.Fatal("the plugin never logged, so nothing proved its logger usable")
	}
}
