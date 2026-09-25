package registry

import (
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/secrets"
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
		Secrets:      []lore.Secret{{Key: "api_key", DefaultEnv: "ACME_API_KEY"}},
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
	const declared = "fake-token-123456789"
	t.Setenv("ACME_API_KEY", "fake-default-key-987654321")
	t.Setenv("LORE_OPENROUTER_KEY", declared)

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
			"base_url": "https://openrouter.ai/api",
			"api_key":  "${env:LORE_OPENROUTER_KEY}",
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

	if got.Secret("api_key") != declared {
		t.Errorf("api_key = %q, want the declared field's value", got.Secret("api_key"))
	}
	if want := `{"base_url":"https://openrouter.ai/api"}`; string(got.Config) != want {
		t.Errorf("config = %s, want %s with no secret key or value", got.Config, want)
	}
}

func TestBuildSourcesDeliversASecretWrittenAsALiteralOrAnExpansion(t *testing.T) {
	const jiraURL = "https://jira.acme.dev"
	values := map[string]string{"token": "fake-token-123456789", "email": "dev@acme.dev"}
	t.Setenv("LORE_ACME_TOKEN", values["token"])
	t.Setenv("LORE_ACME_EMAIL", values["email"])
	t.Setenv("ACME_TOKEN", "fake-default-token-987654321")

	forms := map[string]map[string]any{
		"literal":   {"token": values["token"], "email": values["email"]},
		"expansion": {"token": "${env:LORE_ACME_TOKEN}", "email": "${env:LORE_ACME_EMAIL}"},
	}
	manifests := map[string][]lore.Secret{
		"one secret":                    {{Key: "token", DefaultEnv: "ACME_TOKEN"}},
		"two secrets, as jira declares": {{Key: "token", DefaultEnv: "ACME_TOKEN"}, {Key: "email"}},
	}
	for manifestName, declared := range manifests {
		for form, held := range forms {
			t.Run(manifestName+" as a "+form, func(t *testing.T) {
				var got lore.SourceConfig
				plugin := tokenSource(&got)
				plugin.manifest.Fields = []lore.Field{{Name: "base_url", Type: lore.FieldURL}}
				plugin.manifest.Secrets = declared

				with := map[string]any{"base_url": jiraURL}
				for _, s := range declared {
					with[s.Key] = held[s.Key]
				}
				if _, err := newRegistry(t, plugin).BuildSources([]Instance{{Use: "acme", With: with, Field: "sources[acme]"}}); err != nil {
					t.Fatalf("BuildSources: %v", err)
				}

				for _, s := range declared {
					if got.Secret(s.Key) != values[s.Key] {
						t.Errorf("%s = %q, want %q", s.Key, got.Secret(s.Key), values[s.Key])
					}
				}
				if want := `{"base_url":"` + jiraURL + `"}`; string(got.Config) != want {
					t.Errorf("config = %s, want %s with no secret key or value", got.Config, want)
				}
			})
		}
	}
}

func TestBuildProviderRefusesADeclaredKeyExpandingAnUnsetVariable(t *testing.T) {
	t.Setenv("ACME_API_KEY", "fake-default-key-987654321")
	unsetEnv(t, "OPENAI_API_KEY")

	built := false
	r := newRegistry(t, stubProvider{
		manifest: providerManifest("acme", lore.Capabilities{Complete: true}),
		build: func(lore.ProviderConfig) (lore.Provider, error) {
			built = true
			return completeOnly{}, nil
		},
	})

	_, err := r.BuildProvider(Binding{
		Provider:   "openai",
		Model:      "gpt-4o-mini",
		Capability: lore.CapabilityComplete,
		Field:      "llm",
	}, []Instance{{
		ID:    "openai",
		Use:   "acme",
		With:  map[string]any{"api_key": "${env:OPENAI_API_KEY}"},
		Field: "providers[openai]",
	}})
	if err == nil {
		t.Fatal("BuildProvider: want a refusal rather than the plugin's default variable")
	}
	want := "providers[openai].with.api_key expands OPENAI_API_KEY, but OPENAI_API_KEY is not set"
	if message := internalerror.MessageOf(err); message != want {
		t.Errorf("refusal %q, want %q", message, want)
	}
	if built {
		t.Error("the plugin was built without its declared credential")
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
		Secrets: []lore.Secret{{Key: "token", DefaultEnv: "ACME_TOKEN"}},
	}

	const windowVar = "LORE_ACME_WINDOW"

	tests := []struct {
		name   string
		with   map[string]any
		want   []string
		absent string
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
			name:   "url that no request can be built from",
			with:   map[string]any{"base_url": "intranet.acme"},
			want:   []string{"sources[acme].with.base_url must be an absolute http(s) URL like https://acme.dev"},
			absent: "intranet.acme",
		},
		{
			name: "list holding something that is not a string",
			with: map[string]any{"base_url": "https://acme.dev", "projects": []any{"P", 7}},
			want: []string{"sources[acme].with.projects[1] must be a string"},
		},
		{
			name:   "scalar where a list is declared",
			with:   map[string]any{"base_url": "https://acme.dev", "projects": "PROJ-ALPHA"},
			want:   []string{"sources[acme].with.projects must be a list of strings"},
			absent: "PROJ-ALPHA",
		},
		{
			name: "fractional value for a whole number",
			with: map[string]any{"base_url": "https://acme.dev", "page_size": 1.5},
			want: []string{"sources[acme].with.page_size must be a whole number"},
		},
		{
			name:   "string where a boolean is declared",
			with:   map[string]any{"base_url": "https://acme.dev", "verbose": "affirmative"},
			want:   []string{"sources[acme].with.verbose must be true or false"},
			absent: "affirmative",
		},
		{
			name:   "unparseable duration",
			with:   map[string]any{"base_url": "https://acme.dev", "window": "a fortnight"},
			want:   []string{`sources[acme].with.window must be a duration like "30m" or "30d"`},
			absent: "fortnight",
		},
		{
			name:   "unparseable duration read from a variable",
			with:   map[string]any{"base_url": "https://acme.dev", "window": "${env:" + windowVar + "}"},
			want:   []string{`sources[acme].with.window (from ${env:` + windowVar + `}) must be a duration like "30m" or "30d"`},
			absent: "fortnight",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ACME_TOKEN", "t-example")
			t.Setenv(windowVar, "a fortnight")

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
			if tt.absent != "" && strings.Contains(err.Error(), tt.absent) {
				t.Errorf("error %q quotes the refused value %q", err, tt.absent)
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
			want: "base_url must be an absolute http(s) URL like https://acme.dev",
		},
		{
			name: "a host no parser accepts",
			raw:  "http:// acme.dev",
			want: "base_url must be an absolute http(s) URL like https://acme.dev",
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
			if message := internalerror.MessageOf(err); message != tt.want {
				t.Errorf("refusal %q, want %q with no quoted value", message, tt.want)
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

func TestPrepareRefusesAnOmittedSecretWhoseDefaultVariableHoldsNothing(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"unset": func(t *testing.T) { unsetEnv(t, "ACME_TOKEN") },
		"blank": func(t *testing.T) { t.Setenv("ACME_TOKEN", " \t") },
	}
	for name, empty := range cases {
		t.Run(name, func(t *testing.T) {
			empty(t)

			_, err := newRegistry(t, tokenSource(nil)).BuildSources([]Instance{{Use: "acme", Field: "sources[acme]"}})
			if err == nil {
				t.Fatal("BuildSources: want an error")
			}
			want := "sources[acme].with.token is not set, and its default variable ACME_TOKEN is not set or is blank; " +
				"export ACME_TOKEN, or set the field, as a value or as `${env:VAR}`"
			if message := internalerror.MessageOf(err); !strings.Contains(message, want) {
				t.Errorf("refusal %q does not contain %q", message, want)
			}
			if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
				t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
			}
		})
	}
}

func TestBuildSourcesRecordsEachResolvedSecretWithTheSink(t *testing.T) {
	const (
		literalToken  = "fake-token-literal-123456789"
		expanded      = "fake-token-expanded-123456789"
		fallbackToken = "fake-token-default-123456789"
	)
	t.Setenv(tokenVar, expanded)
	t.Setenv("ACME_TOKEN", fallbackToken)

	sink := &secrets.Sink{}
	r := New(lore.Host{}, sink)
	if err := r.Register(tokenSource(nil)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	instances := []Instance{
		{ID: "pasted", Use: "acme", With: map[string]any{"token": literalToken}, Field: "sources[pasted]"},
		{ID: "expanded", Use: "acme", With: map[string]any{"token": "${env:" + tokenVar + "}"}, Field: "sources[expanded]"},
		{ID: "fallback", Use: "acme", Field: "sources[fallback]"},
	}
	for range 2 {
		if _, err := r.BuildSources(instances); err != nil {
			t.Fatalf("BuildSources: %v", err)
		}
	}

	for _, value := range []string{literalToken, expanded, fallbackToken} {
		if got := sink.Scrub("auth " + value); got != "auth "+secrets.Placeholder {
			t.Errorf("Scrub = %q, want the resolved value redacted", got)
		}
	}
	notices := sink.Notices()
	if len(notices) != 1 {
		t.Fatalf("notices = %q, want one line for the literal", notices)
	}
	notice := notices[0]
	if got := strings.Count(notice, "sources[pasted].with.token"); got != 1 {
		t.Errorf("notice %q names the literal field %d times, want once", notice, got)
	}
	if !strings.Contains(notice, "${env:VAR}") {
		t.Errorf("notice %q does not point at the expansion form", notice)
	}
	for _, unnamed := range []string{"sources[expanded]", "sources[fallback]", "ACME_TOKEN", literalToken} {
		if strings.Contains(notice, unnamed) {
			t.Errorf("notice %q names %q, want only the literal field", notice, unnamed)
		}
	}
}

func TestPrepareRefusesASecretFieldHoldingNoCredential(t *testing.T) {
	t.Setenv("ACME_TOKEN", "fake-token-default-123456789")
	t.Setenv(tokenVar, " \t")

	const mustHold = "sources[acme].with.token must hold the token as a value or as `${env:VAR}`"
	cases := map[string]struct {
		held any
		want string
	}{
		"blank literal":                   {held: " \t", want: mustHold},
		"expansion of a blank variable":   {held: "${env:" + tokenVar + "}", want: "sources[acme].with.token expands " + tokenVar + ", but it is blank"},
		"number where a string is needed": {held: 123456789, want: mustHold},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newRegistry(t, tokenSource(nil)).BuildSources([]Instance{{
				Use:   "acme",
				With:  map[string]any{"token": tt.held},
				Field: "sources[acme]",
			}})
			if err == nil {
				t.Fatal("BuildSources: want a refusal rather than the default variable")
			}
			if message := internalerror.MessageOf(err); message != tt.want {
				t.Errorf("refusal %q, want %q", message, tt.want)
			}
		})
	}
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

func TestBuildCodeRefusesAPluginThatBuiltNoAccessor(t *testing.T) {
	r := newRegistry(t, codePlugin{
		manifest: codeManifest("git"),
		build: func(lore.CodeConfig) (lore.CodeRepo, error) {
			return nil, nil
		},
	})

	_, err := r.BuildCode([]LocalClone{{Path: "/w/app", Use: "git", Field: "repos[0]"}})
	if err == nil {
		t.Fatal("BuildCode: want an error instead of an accessor nothing can call")
	}
	for _, want := range []string{`plugin "git"`, "built no accessor", "/w/app"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if got := internalerror.KindOf(err); got != internalerror.KindInternal {
		t.Errorf("kind = %s, want %s", got, internalerror.KindInternal)
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

func tokenSource(capture *lore.SourceConfig) stubSource {
	manifest := sourceManifest("acme")
	manifest.Secrets = []lore.Secret{{Key: "token", DefaultEnv: "ACME_TOKEN"}}
	return stubSource{manifest: manifest, build: func(c lore.SourceConfig) (lore.Connector, error) {
		if capture != nil {
			*capture = c
		}
		return stubConnector{name: c.Instance}, nil
	}}
}

func externalRegistry(t *testing.T, plugin lore.Plugin) *Registry {
	t.Helper()

	r := New(lore.Host{}, nil)
	if err := r.RegisterExternal(OriginExternal("./bin/lore-acme"), plugin.Manifest().Name, plugin); err != nil {
		t.Fatalf("RegisterExternal: %v", err)
	}
	return r
}

func TestPrepareIgnoresAnExternallyInstalledPluginsSecretDefault(t *testing.T) {
	const granted = "fake-token-123456789"
	t.Setenv("ACME_TOKEN", granted)

	r := externalRegistry(t, tokenSource(nil))

	_, err := r.BuildSources([]Instance{{Use: "acme", Field: "sources[acme]"}})
	if err == nil {
		t.Fatal("BuildSources: want an error even though the declared variable holds a value")
	}
	message := internalerror.MessageOf(err)
	for _, want := range []string{
		"sources[acme].with.token must hold the token as a value or as `${env:VAR}`",
		"a plugin installed from outside the binary cannot choose a default",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("refusal %q does not contain %q", message, want)
		}
	}
	for _, suggested := range []string{"ACME_TOKEN", granted} {
		if strings.Contains(message, suggested) {
			t.Errorf("refusal %q steers the operator onto the plugin's default: %q", message, suggested)
		}
	}
	if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
		t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
	}
}

func TestPrepareUsesACompiledInPluginsSecretDefault(t *testing.T) {
	t.Setenv("ACME_TOKEN", "t-example")

	var got lore.SourceConfig
	r := newRegistry(t, tokenSource(&got))

	if _, err := r.BuildSources([]Instance{{Use: "acme", Field: "sources[acme]"}}); err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	if got.Secret("token") != "t-example" {
		t.Errorf("token = %q, want the value of the manifest's declared variable", got.Secret("token"))
	}
}

func keylessProvider(capture *lore.ProviderConfig) stubProvider {
	manifest := providerManifest("acme", lore.Capabilities{Complete: true})
	manifest.Secrets = []lore.Secret{{Key: "api_key", Optional: true}}
	return stubProvider{manifest: manifest, build: func(c lore.ProviderConfig) (lore.Provider, error) {
		if capture != nil {
			*capture = c
		}
		return completeOnly{}, nil
	}}
}

func TestBuildProviderNeedsACredentialOnlyForANonOptionalSecret(t *testing.T) {
	instances := []Instance{{ID: "local", Use: "acme", Field: "providers[local]"}}
	binding := Binding{
		Provider:   "local",
		Model:      "qwen3-8b",
		Capability: lore.CapabilityComplete,
		Field:      "llm",
	}

	buildKeyless := func(t *testing.T, defaultEnv string) lore.ProviderConfig {
		t.Helper()

		var got lore.ProviderConfig
		plugin := keylessProvider(&got)
		plugin.manifest.Secrets[0].DefaultEnv = defaultEnv

		built, err := newRegistry(t, plugin).BuildProvider(binding, instances)
		if err != nil {
			t.Fatalf("BuildProvider: %v", err)
		}
		if _, ok := built.Value.(lore.Completer); !ok {
			t.Fatalf("built %T, want a lore.Completer", built.Value)
		}
		return got
	}

	t.Run("optional and no default variable", func(t *testing.T) {
		if key := buildKeyless(t, "").Secret("api_key"); key != "" {
			t.Errorf("api_key = %q, want the plugin to receive no value", key)
		}
	})

	t.Run("the same manifest without the optional marker", func(t *testing.T) {
		plugin := keylessProvider(nil)
		plugin.manifest.Secrets[0].Optional = false
		plugin.manifest.Secrets[0].Doc = "a key for the local gateway."
		r := newRegistry(t, plugin)

		_, err := r.BuildProvider(binding, instances)
		if err == nil {
			t.Fatal("BuildProvider: want an error")
		}
		want := "providers[local].with.api_key must hold the api_key as a value or as `${env:VAR}` — a key for the local gateway"
		if message := internalerror.MessageOf(err); message != want {
			t.Errorf("refusal %q, want %q", message, want)
		}
		if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
			t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
		}
	})

	const apiKeyEnv = "LORE_ACME_API_KEY"

	t.Run("optional with a manifest default whose variable is unset", func(t *testing.T) {
		t.Setenv(apiKeyEnv, "")

		if key := buildKeyless(t, apiKeyEnv).Secret("api_key"); key != "" {
			t.Errorf("api_key = %q, want the plugin to receive no value", key)
		}
	})

	t.Run("optional with a manifest default whose variable holds a value", func(t *testing.T) {
		t.Setenv(apiKeyEnv, "k-example")

		if key := buildKeyless(t, apiKeyEnv).Secret("api_key"); key != "k-example" {
			t.Errorf("api_key = %q, want the value of the manifest's declared variable", key)
		}
	})

	t.Run("optional but written blank", func(t *testing.T) {
		declared := []Instance{{ID: "local", Use: "acme", Field: "providers[local]", With: map[string]any{"api_key": " \t"}}}

		_, err := newRegistry(t, keylessProvider(nil)).BuildProvider(binding, declared)
		if err == nil {
			t.Fatal("BuildProvider: want a refusal of a blank value rather than skipping the secret")
		}
		if want := "providers[local].with.api_key must hold the api_key as a value or as `${env:VAR}`"; internalerror.MessageOf(err) != want {
			t.Errorf("refusal %q, want %q", internalerror.MessageOf(err), want)
		}
	})
}

func TestPrepareRefusalsNeverEchoTheCredential(t *testing.T) {
	const pasted = "fk-9zQ4XmT7pLvB2sNc"
	unsetEnv(t, missingVar)

	plugin := tokenSource(nil)
	plugin.manifest.Secrets = []lore.Secret{{Key: "token"}, {Key: "email"}}
	r := newRegistry(t, plugin)

	tests := []struct {
		name string
		with map[string]any
		want string
	}{
		{
			name: "the other secret omitted",
			with: map[string]any{"token": pasted},
			want: "sources[acme].with.email must hold the email as a value or as `${env:VAR}`",
		},
		{
			name: "the other secret expands an unset variable",
			with: map[string]any{"token": pasted, "email": "${env:" + missingVar + "}"},
			want: "sources[acme].with.email expands " + missingVar + ", but " + missingVar + " is not set",
		},
		{
			name: "the credential under a key the plugin does not accept",
			with: map[string]any{"tokne": pasted, "email": "dev@acme.dev"},
			want: "sources[acme].with.tokne is not a key plugin \"acme\" accepts",
		},
		{
			name: "the credential inside a list",
			with: map[string]any{"token": []any{pasted}, "email": "dev@acme.dev"},
			want: "sources[acme].with.token must hold the token",
		},
		{
			name: "the credential beside a malformed expansion",
			with: map[string]any{"token": pasted + "${" + missingVar + "}", "email": "dev@acme.dev"},
			want: "sources[acme].with.token holds a ${ that is not ${env:VAR}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := r.BuildSources([]Instance{{Use: "acme", With: tt.with, Field: "sources[acme]"}})
			if err == nil {
				t.Fatal("BuildSources: want an error")
			}

			message := internalerror.MessageOf(err)
			if !strings.Contains(message, tt.want) {
				t.Errorf("refusal %q does not contain %q", message, tt.want)
			}
			const fragment = 4
			for i := 0; i+fragment <= len(pasted); i++ {
				if part := pasted[i : i+fragment]; strings.Contains(message, part) {
					t.Errorf("refusal %q echoes %q from the credential", message, part)
				}
			}
			if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
				t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
			}
		})
	}
}

func TestRegisterExternalRefusesAnOriginItCannotTrust(t *testing.T) {
	cases := map[string]string{
		"no origin at all":            "",
		"a forged compiled-in origin": OriginBuiltin,
	}

	for name, origin := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRegistry(t, stubSource{manifest: sourceManifest("acme")})

			err := r.RegisterExternal(origin, "ghost", stubSource{manifest: sourceManifest("ghost")})
			if err == nil {
				t.Fatal("RegisterExternal: want an error")
			}
			for _, want := range []string{`plugin "ghost"`, "must carry its own origin", "cannot claim compiled-in trust"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if got := internalerror.KindOf(err); got != internalerror.KindInternal {
				t.Errorf("kind = %s, want %s", got, internalerror.KindInternal)
			}
			if _, ok := r.Manifest("ghost"); ok {
				t.Error("the refused plugin is resolvable by name")
			}
			if entries := r.List(); len(entries) != 1 || entries[0].Manifest.Name != "acme" {
				t.Errorf("entries = %+v, want only the compiled-in plugin", entries)
			}
		})
	}
}

func TestCheckDeclarationsChecksAnInstanceNoRoleBinds(t *testing.T) {
	r := newRegistry(t, honest(lore.Capabilities{Complete: true}), stubSource{manifest: sourceManifest("ghost")})

	refused := func(t *testing.T, in Instance, want ...string) {
		t.Helper()

		err := r.CheckDeclarations([]Instance{in}, lore.KindProvider)
		if err == nil {
			t.Fatal("CheckDeclarations: want an error")
		}
		for _, fragment := range want {
			if !strings.Contains(err.Error(), fragment) {
				t.Errorf("error %q does not contain %q", err, fragment)
			}
		}
		if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
			t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
		}
	}

	t.Run("a plugin name nothing registers", func(t *testing.T) {
		refused(t, Instance{ID: "openrouter", Use: "acmee", Field: "providers[openrouter]"},
			`providers[openrouter].use names "acmee"`, "neither a compiled plugin")
	})

	t.Run("a with key the plugin does not accept", func(t *testing.T) {
		refused(t, Instance{
			ID:    "openrouter",
			Use:   "acme",
			With:  map[string]any{"bse_url": "https://openrouter.ai/api"},
			Field: "providers[openrouter]",
		}, "providers[openrouter].with.bse_url", "is not a key", "base_url")
	})

	t.Run("a plugin of another kind", func(t *testing.T) {
		refused(t, Instance{ID: "ghost", Use: "ghost", Field: "providers[ghost]"},
			`providers[ghost].use names "ghost"`, "is a source plugin, not a provider plugin")
	})

	t.Run("a well-formed instance whose secret variable is unset", func(t *testing.T) {
		t.Setenv("ACME_API_KEY", "")

		in := Instance{
			ID:    "openrouter",
			Use:   "acme",
			With:  map[string]any{"base_url": "https://openrouter.ai/api"},
			Field: "providers[openrouter]",
		}
		if err := r.CheckDeclarations([]Instance{in}, lore.KindProvider); err != nil {
			t.Errorf("CheckDeclarations: %v, want an unbound instance to need no exported variable", err)
		}
	})
}

func suggestedProvider(t *testing.T, message string) (config.Instance, map[string]any) {
	t.Helper()

	_, quoted, opened := strings.Cut(message, "`")
	snippet, _, closed := strings.Cut(quoted, "`")
	if !opened || !closed {
		t.Fatalf("refusal %q quotes no configuration to add", message)
	}

	cfg, err := config.Decode(strings.NewReader(snippet))
	if err != nil {
		t.Fatalf("the suggested %q is not loadable configuration: %v", snippet, err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("the suggested %q declares %d provider instances, want one", snippet, len(cfg.Providers))
	}
	with, err := cfg.Providers[0].WithValues()
	if err != nil {
		t.Fatalf("the suggested %q carries an unreadable with block: %v", snippet, err)
	}
	return cfg.Providers[0], with
}

func TestBuildProviderRefusesAnImplicitInstanceWithConfigurationTheOperatorCanAdd(t *testing.T) {
	binding := Binding{
		Provider:   "acme",
		Model:      "acme-embed",
		Capability: lore.CapabilityEmbed,
		Field:      "embedder",
	}

	cases := map[string]string{
		"the secret names no default variable": "",
		"the default variable is not set":      "ACME_API_KEY",
	}

	for name, defaultEnv := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ACME_API_KEY", "")

			plugin := honest(lore.Capabilities{Embed: true})
			plugin.manifest.Secrets[0].DefaultEnv = defaultEnv

			_, err := newRegistry(t, plugin).BuildProvider(binding, nil)
			if err == nil {
				t.Fatal("BuildProvider: want an error")
			}

			message := internalerror.MessageOf(err)
			if strings.Contains(message, "embedder.provider.with") {
				t.Errorf("refusal %q names a key no role binding accepts", message)
			}

			suggested, with := suggestedProvider(t, message)
			if suggested.ID != binding.Provider || suggested.Use != binding.Provider {
				t.Errorf("suggested instance has id %q and use %q, want both %q so %s still resolves",
					suggested.ID, suggested.Use, binding.Provider, binding.Field)
			}
			if got, want := with["api_key"], "${env:YOUR_VARIABLE}"; got != want {
				t.Errorf("suggested with block %v sets api_key to %v, want the expansion %q", with, got, want)
			}
			if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
				t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
			}
		})
	}
}

func TestPrepareRejectsAnEmptyListForARequiredScopeKey(t *testing.T) {
	scopedSource := func(required bool) stubSource {
		manifest := sourceManifest("acme")
		manifest.Fields = []lore.Field{{Name: "projects", Type: lore.FieldStringList, Required: required}}
		return stubSource{manifest: manifest}
	}
	instances := []Instance{{Use: "acme", With: map[string]any{"projects": []any{}}, Field: "sources[acme]"}}

	t.Run("the plugin marks the key required", func(t *testing.T) {
		_, err := newRegistry(t, scopedSource(true)).BuildSources(instances)
		if err == nil {
			t.Fatal("BuildSources: want an error instead of an instance that ingests nothing")
		}
		for _, want := range []string{"sources[acme].with.projects", "must list at least one entry"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
		if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
			t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
		}
	})

	t.Run("the plugin leaves the key optional", func(t *testing.T) {
		built, err := newRegistry(t, scopedSource(false)).BuildSources(instances)
		if err != nil {
			t.Fatalf("BuildSources: %v", err)
		}
		if len(built) != 1 {
			t.Errorf("built %d connectors, want the instance to start and scope itself to everything", len(built))
		}
	})
}
