package registry

import (
	"os"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

const (
	urlVar     = "LORE_EXPAND_URL"
	teamVar    = "LORE_EXPAND_TEAM"
	tokenVar   = "LORE_EXPAND_TOKEN"
	missingVar = "LORE_EXPAND_MISSING"

	gatewayURL    = "https://gateway.acme.dev"
	expandedToken = "fake-token-from-env"
)

func unsetEnv(t *testing.T, name string) {
	t.Helper()

	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
}

// built stays nil until the plugin is asked to build, so a refusal can prove the plugin never saw the config.
func expandingSource(built **lore.SourceConfig) stubSource {
	manifest := sourceManifest("acme")
	manifest.Fields = []lore.Field{
		{Name: "base_url", Type: lore.FieldURL},
		{Name: "labels", Type: lore.FieldStringList},
		{Name: "query", Type: lore.FieldString},
	}
	manifest.Secrets = []lore.Secret{{Key: "token"}}
	return stubSource{manifest: manifest, build: func(c lore.SourceConfig) (lore.Connector, error) {
		*built = &c
		return stubConnector{name: c.Instance}, nil
	}}
}

func TestBuildSourcesExpandsACompiledInPluginsOrdinaryField(t *testing.T) {
	t.Setenv(urlVar, gatewayURL)
	t.Setenv(tokenVar, expandedToken)

	var built *lore.SourceConfig
	r := newRegistry(t, expandingSource(&built))

	_, err := r.BuildSources([]Instance{{Use: "acme", Field: "sources[acme]", With: map[string]any{
		"base_url": "${env:" + urlVar + "}",
		"token":    "${env:" + tokenVar + "}",
	}}})
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	if got, want := string(built.Config), `{"base_url":"`+gatewayURL+`"}`; got != want {
		t.Errorf("config = %s, want %s", got, want)
	}
	if got := built.Secret("token"); got != expandedToken {
		t.Errorf("token = %q, want %q", got, expandedToken)
	}
}

func TestBuildSourcesExpandsInsideAStringListWithoutEditingTheInstance(t *testing.T) {
	t.Setenv(teamVar, "support")

	var built *lore.SourceConfig
	r := newRegistry(t, expandingSource(&built))
	labels := []any{"crm", "team-${env:" + teamVar + "}"}

	_, err := r.BuildSources([]Instance{{Use: "acme", Field: "sources[acme]", With: map[string]any{
		"labels": labels,
		"token":  "fake-token",
	}}})
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	if got, want := string(built.Config), `{"labels":["crm","team-support"]}`; got != want {
		t.Errorf("config = %s, want %s", got, want)
	}
	if labels[1] != "team-${env:"+teamVar+"}" {
		t.Errorf("the declared instance was edited in place: labels[1] = %q", labels[1])
	}
}

func TestBuildSourcesPassesAnEscapedExpansionThroughAsText(t *testing.T) {
	unsetEnv(t, missingVar)

	registries := map[string]func(*testing.T, lore.Plugin) *Registry{
		"compiled in": func(t *testing.T, p lore.Plugin) *Registry { return newRegistry(t, p) },
		"external":    externalRegistry,
	}
	for name, register := range registries {
		t.Run(name, func(t *testing.T) {
			var built *lore.SourceConfig
			r := register(t, expandingSource(&built))

			_, err := r.BuildSources([]Instance{{Use: "acme", Field: "sources[acme]", With: map[string]any{
				"query": "$${env:" + missingVar + "}",
				"token": "fake-token",
			}}})
			if err != nil {
				t.Fatalf("BuildSources: %v; an escaped expansion must not read %s, which is unset", err, missingVar)
			}
			if got, want := string(built.Config), `{"query":"${env:`+missingVar+`}"}`; got != want {
				t.Errorf("config = %s, want %s", got, want)
			}
		})
	}
}

func TestBuildSourcesRefusesAnExpansionOfAnUnsetVariable(t *testing.T) {
	t.Setenv(urlVar, gatewayURL)
	unsetEnv(t, missingVar)

	tests := []struct {
		name  string
		with  map[string]any
		field string
	}{
		{
			name:  "ordinary field",
			with:  map[string]any{"base_url": "${env:" + missingVar + "}", "token": "fake-token"},
			field: "sources[acme].with.base_url",
		},
		{
			name:  "secret field",
			with:  map[string]any{"base_url": "${env:" + urlVar + "}", "token": "${env:" + missingVar + "}"},
			field: "sources[acme].with.token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var built *lore.SourceConfig
			r := newRegistry(t, expandingSource(&built))

			_, err := r.BuildSources([]Instance{{Use: "acme", Field: "sources[acme]", With: tt.with}})
			if err == nil {
				t.Fatal("BuildSources: want an error")
			}
			if want := tt.field + " expands " + missingVar + ", but " + missingVar + " is not set"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
			if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
				t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
			}
			if built != nil {
				t.Error("the plugin was built from a configuration holding an unset expansion")
			}
		})
	}
}

func TestBuildSourcesExpandsAnExternalPluginsSecretField(t *testing.T) {
	t.Setenv(tokenVar, expandedToken)

	var built *lore.SourceConfig
	r := externalRegistry(t, expandingSource(&built))

	_, err := r.BuildSources([]Instance{{Use: "acme", Field: "sources[acme]", With: map[string]any{
		"query": "is:open",
		"token": "${env:" + tokenVar + "}",
	}}})
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	if got := built.Secret("token"); got != expandedToken {
		t.Errorf("token = %q, want %q", got, expandedToken)
	}
	if got, want := string(built.Config), `{"query":"is:open"}`; got != want {
		t.Errorf("config = %s, want %s", got, want)
	}
}

var sourceEntryPoints = map[string]func(*Registry, []Instance) error{
	"BuildSources": func(r *Registry, instances []Instance) error {
		_, err := r.BuildSources(instances)
		return err
	},
	"CheckDeclarations": func(r *Registry, instances []Instance) error {
		return r.CheckDeclarations(instances, lore.KindSource)
	},
}

func TestExternalPluginRefusesAnExpansionOutsideItsSecretFields(t *testing.T) {
	const leaked = "https://leaked.acme.dev"

	fields := []struct {
		name  string
		with  map[string]any
		field string
		key   string
	}{
		{
			name:  "scalar",
			with:  map[string]any{"base_url": "${env:" + urlVar + "}", "token": "fake-token"},
			field: "sources[acme].with.base_url",
			key:   "base_url",
		},
		{
			name:  "string list entry",
			with:  map[string]any{"labels": []any{"crm", "team-${env:" + urlVar + "}"}, "token": "fake-token"},
			field: "sources[acme].with.labels[1]",
			key:   "labels",
		},
	}

	for entry, call := range sourceEntryPoints {
		for _, f := range fields {
			t.Run(entry+"/"+f.name, func(t *testing.T) {
				var built *lore.SourceConfig
				r := externalRegistry(t, expandingSource(&built))
				instances := []Instance{{Use: "acme", Field: "sources[acme]", With: f.with}}

				t.Setenv(urlVar, leaked)
				withValue := call(r, instances)
				unsetEnv(t, urlVar)
				withoutValue := call(r, instances)

				if withValue == nil {
					t.Fatal("want an error")
				}
				for _, want := range []string{
					f.field, `plugin "acme"`, "installed from outside the binary",
					"does not mark " + f.key + " expandable", "only the plugin's author can mark it",
				} {
					if !strings.Contains(withValue.Error(), want) {
						t.Errorf("error %q does not contain %q", withValue, want)
					}
				}
				if strings.Contains(withValue.Error(), leaked) {
					t.Errorf("error %q carries the variable's value", withValue)
				}
				if got := internalerror.KindOf(withValue); got != internalerror.KindBadRequest {
					t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
				}
				// Had the variable been read, unsetting it would have changed the refusal into an unset-variable error.
				if withoutValue == nil || withoutValue.Error() != withValue.Error() {
					t.Errorf("refusal with %s unset = %v, want the same refusal as with it set", urlVar, withoutValue)
				}
				if built != nil {
					t.Error("the plugin was built from a refused configuration")
				}
			})
		}
	}
}

func TestExternalPluginRefusesAnExpansionInAnUndeclaredKeyAsAnUnknownKey(t *testing.T) {
	unsetEnv(t, urlVar)

	instances := []Instance{{Use: "acme", Field: "sources[acme]", With: map[string]any{
		"base_ur": "${env:" + urlVar + "}",
		"token":   "fake-token",
	}}}

	for entry, call := range sourceEntryPoints {
		t.Run(entry, func(t *testing.T) {
			var built *lore.SourceConfig
			r := externalRegistry(t, expandingSource(&built))

			err := call(r, instances)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), urlVar) {
				t.Errorf("error %q names %s; the undeclared key's expansion was read", err, urlVar)
			}
			if want := `sources[acme].with.base_ur is not a key plugin "acme" accepts`; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
			if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
				t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
			}
			if built != nil {
				t.Error("the plugin was built from a refused configuration")
			}
		})
	}
}

func TestCheckDeclarationsExpandsAnUnboundProvidersURL(t *testing.T) {
	unsetEnv(t, missingVar)

	r := newRegistry(t, stubProvider{manifest: providerManifest("acme", lore.Capabilities{Complete: true})})
	instances := []Instance{{ID: "gateway", Use: "acme", Field: "providers[gateway]", With: map[string]any{
		"base_url": "${env:" + urlVar + "}",
		"api_key":  "${env:" + missingVar + "}",
	}}}

	t.Run("the expanded value is a URL", func(t *testing.T) {
		t.Setenv(urlVar, gatewayURL)

		if err := r.CheckDeclarations(instances, lore.KindProvider); err != nil {
			t.Fatalf("CheckDeclarations: %v; the unexpanded text is not a URL and the secret is never resolved here", err)
		}
	})

	refusals := map[string]string{
		"a URL of another scheme": "ftp://gateway.acme.dev/fake-path-123456",
		"text url.Parse rejects":  "https://svc:fake-pass-123456@gw acme.dev",
	}
	for name, value := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Setenv(urlVar, value)

			err := r.CheckDeclarations(instances, lore.KindProvider)
			if err == nil {
				t.Fatal("CheckDeclarations: want an error")
			}
			want := "providers[gateway].with.base_url (from ${env:" + urlVar + "}) must be an absolute http(s) URL"
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
			if strings.Contains(err.Error(), "fake-") {
				t.Errorf("error %q carries the variable's value", err)
			}
		})
	}
}

func TestRefusalNamesTheVariableBehindTheValueItRefuses(t *testing.T) {
	t.Setenv(teamVar, "support")
	t.Setenv(urlVar, "ftp")

	tests := []struct {
		name string
		with map[string]any
		want string
	}{
		{
			name: "a literal list item beside an expanded one names no variable",
			with: map[string]any{"labels": []any{"${env:" + teamVar + "}", 7}},
			want: "sources[acme].with.labels[1] must be a string",
		},
		{
			name: "a variable expanded twice is named once",
			with: map[string]any{"base_url": "${env:" + urlVar + "}://${env:" + urlVar + "}"},
			want: "sources[acme].with.base_url (from ${env:" + urlVar + "}) must be an absolute http(s) URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var built *lore.SourceConfig
			r := newRegistry(t, expandingSource(&built))
			tt.with["token"] = "fake-token"

			_, err := r.BuildSources([]Instance{{Use: "acme", Field: "sources[acme]", With: tt.with}})
			if err == nil {
				t.Fatal("BuildSources: want an error")
			}
			if got := internalerror.MessageOf(err); got != tt.want {
				t.Errorf("refusal = %q, want %q", got, tt.want)
			}
		})
	}
}
