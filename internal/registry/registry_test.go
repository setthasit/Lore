package registry

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

// stubSource is a source plugin whose manifest the tests vary freely: the point
// of every case below is what the registry refuses, not what a real source does.
type stubSource struct {
	manifest lore.Manifest
	build    func(lore.SourceConfig) (lore.Connector, error)
}

func (s stubSource) Manifest() lore.Manifest { return s.manifest }

func (s stubSource) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	if s.build != nil {
		return s.build(c)
	}
	return stubConnector{name: c.Instance}, nil
}

type stubConnector struct{ name string }

func (c stubConnector) Name() string { return c.name }

func (stubConnector) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(func(lore.Batch, error) bool) {}
}

// matchingConnector answers remote questions, as a source declaring
// RepoRemotes must.
type matchingConnector struct {
	stubConnector
	remote string
}

func (c matchingConnector) MatchesRemote(remote string) bool { return remote == c.remote }

type stubProvider struct {
	manifest lore.Manifest
	build    func(lore.ProviderConfig) (lore.Provider, error)
}

func (s stubProvider) Manifest() lore.Manifest { return s.manifest }

func (s stubProvider) NewProvider(c lore.ProviderConfig) (lore.Provider, error) {
	return s.build(c)
}

type embedOnly struct{ dims int }

func (embedOnly) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }
func (e embedOnly) Dimensions() int                                    { return e.dims }

type completeOnly struct{}

func (completeOnly) Complete(context.Context, string, string) (string, error) { return "text", nil }

type stubCode struct{ manifest lore.Manifest }

func (s stubCode) Manifest() lore.Manifest { return s.manifest }

func (stubCode) NewCode(lore.CodeConfig) (lore.CodeRepo, error) { return stubRepo{}, nil }

// codePlugin lets a test observe the CodeConfig the registry assembled.
type codePlugin struct {
	manifest lore.Manifest
	build    func(lore.CodeConfig) (lore.CodeRepo, error)
}

func (c codePlugin) Manifest() lore.Manifest { return c.manifest }

func (c codePlugin) NewCode(cfg lore.CodeConfig) (lore.CodeRepo, error) { return c.build(cfg) }

type stubRepo struct{}

func (stubRepo) Blame(context.Context, string, int, int) ([]lore.BlameSpan, error) { return nil, nil }
func (stubRepo) Log(context.Context, string) ([]lore.CommitRef, error)             { return nil, nil }
func (stubRepo) HasFileAtHEAD(context.Context, string) (bool, error)               { return false, nil }

func sourceManifest(name string) lore.Manifest {
	return lore.Manifest{
		Name:       name,
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "a source that exists only in this test",
	}
}

func newRegistry(t *testing.T, plugins ...lore.Plugin) *Registry {
	t.Helper()

	r := New(lore.Host{})
	if err := r.Register(plugins...); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return r
}

func TestRegisterRejectsAMisdeclaredManifest(t *testing.T) {
	sourceWith := func(mutate func(*lore.Manifest)) lore.Plugin {
		m := sourceManifest("acme")
		mutate(&m)
		return stubSource{manifest: m}
	}

	tests := []struct {
		name   string
		plugin lore.Plugin
		want   string
	}{
		{
			name:   "no name",
			plugin: sourceWith(func(m *lore.Manifest) { m.Name = "" }),
			want:   "declares no name",
		},
		{
			name:   "name is not lower-case and hyphenated",
			plugin: sourceWith(func(m *lore.Manifest) { m.Name = "Acme_CRM" }),
			want:   `must be lower-case and hyphenated`,
		},
		{
			name:   "api version the host does not speak",
			plugin: sourceWith(func(m *lore.Manifest) { m.APIVersion = lore.APIVersion + 1 }),
			want:   "speaks api_version 2, host speaks 1",
		},
		{
			name:   "no summary",
			plugin: sourceWith(func(m *lore.Manifest) { m.Summary = "" }),
			want:   "declares no summary",
		},
		{
			name:   "unknown kind",
			plugin: sourceWith(func(m *lore.Manifest) { m.Kind = "secrets" }),
			want:   `declares kind "secrets"`,
		},
		{
			name:   "kind does not match the interface",
			plugin: stubCode{manifest: sourceManifest("acme")},
			want:   "does not implement the matching constructor",
		},
		{
			name: "source claims a model capability",
			plugin: sourceWith(func(m *lore.Manifest) {
				m.Capabilities = lore.Capabilities{Embed: true}
			}),
			want: "declares a model capability",
		},
		{
			name: "field name is not snake_case",
			plugin: sourceWith(func(m *lore.Manifest) {
				m.Fields = []lore.Field{{Name: "BaseURL", Type: lore.FieldURL}}
			}),
			want: "a field name must be snake_case",
		},
		{
			name: "field type is not one of the declared types",
			plugin: sourceWith(func(m *lore.Manifest) {
				m.Fields = []lore.Field{{Name: "base_url", Type: "hostname"}}
			}),
			want: `with type "hostname"`,
		},
		{
			name: "secret config field does not end in _env",
			plugin: sourceWith(func(m *lore.Manifest) {
				m.Secrets = []lore.Secret{{Key: "token", ConfigField: "token"}}
			}),
			want: "must be snake_case and end in _env",
		},
		{
			name: "secret default env is not a variable name",
			plugin: sourceWith(func(m *lore.Manifest) {
				m.Secrets = []lore.Secret{{Key: "token", ConfigField: "token_env", DefaultEnv: "lore-token"}}
			}),
			want: "which is not an environment variable name",
		},
		{
			name: "a field and a secret claim the same key",
			plugin: sourceWith(func(m *lore.Manifest) {
				m.Fields = []lore.Field{{Name: "token_env", Type: lore.FieldString}}
				m.Secrets = []lore.Secret{{Key: "token", ConfigField: "token_env"}}
			}),
			want: `declares "token_env" twice`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := New(lore.Host{}).Register(tt.plugin)
			if err == nil {
				t.Fatal("Register: want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestRegisterRejectsAProviderThatServesNothing(t *testing.T) {
	err := New(lore.Host{}).Register(stubProvider{manifest: lore.Manifest{
		Name:       "acme",
		Kind:       lore.KindProvider,
		APIVersion: lore.APIVersion,
		Summary:    "a provider serving no capability",
	}})
	if err == nil {
		t.Fatal("Register: want an error")
	}
	if !strings.Contains(err.Error(), "declares neither embed nor complete") {
		t.Errorf("error %q does not say the provider serves nothing", err)
	}
}

func TestRegisterRejectsADuplicateName(t *testing.T) {
	err := New(lore.Host{}).Register(
		stubSource{manifest: sourceManifest("acme")},
		stubSource{manifest: sourceManifest("acme")},
	)
	if err == nil {
		t.Fatal("Register: want an error")
	}
	if !strings.Contains(err.Error(), "registered twice") {
		t.Errorf("error %q does not name the collision", err)
	}
}

func TestBuildSourcesNamesEveryInstanceAfterItsID(t *testing.T) {
	r := newRegistry(t, stubSource{manifest: sourceManifest("jira")})

	connectors, err := r.BuildSources([]Instance{
		{Use: "jira", Field: "sources[jira]"},
		{ID: "jira-legacy", Use: "jira", Field: "sources[jira-legacy]"},
	})
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	if len(connectors) != 2 {
		t.Fatalf("built %d connectors, want 2", len(connectors))
	}
	// The id defaults to the plugin name, so a single-instance workspace reads
	// `use: jira` and gets the instance id "jira".
	if got := connectors[0].Name(); got != "jira" {
		t.Errorf("first instance is named %q, want %q", got, "jira")
	}
	if got := connectors[1].Name(); got != "jira-legacy" {
		t.Errorf("second instance is named %q, want %q", got, "jira-legacy")
	}
}

func TestBuildSourcesRejectsAConnectorThatRenamesItself(t *testing.T) {
	r := newRegistry(t, stubSource{
		manifest: sourceManifest("jira"),
		build: func(lore.SourceConfig) (lore.Connector, error) {
			return stubConnector{name: "somewhere-else"}, nil
		},
	})

	_, err := r.BuildSources([]Instance{{ID: "jira-acme", Use: "jira", Field: "sources[jira-acme]"}})
	if err == nil {
		t.Fatal("BuildSources: want an error")
	}
	if !strings.Contains(err.Error(), "may not rename itself") {
		t.Errorf("error %q does not explain why the name is load-bearing", err)
	}
}

// A colon in an instance id would make every DocID it produces unparseable.
func TestBuildSourcesRejectsAnInstanceIDThatWouldCorruptDocumentIdentity(t *testing.T) {
	r := newRegistry(t, stubSource{manifest: sourceManifest("jira")})

	_, err := r.BuildSources([]Instance{{ID: "jira:acme", Use: "jira", Field: "sources[jira:acme]"}})
	if err == nil {
		t.Fatal("BuildSources: want an error")
	}
	if !strings.Contains(err.Error(), "prefix of every document identity") {
		t.Errorf("error %q does not explain why the id shape matters", err)
	}
}

func TestBuildSourcesRejectsARepoRemotesLie(t *testing.T) {
	m := sourceManifest("forge")
	m.Capabilities = lore.Capabilities{RepoRemotes: true}

	r := newRegistry(t, stubSource{manifest: m})

	_, err := r.BuildSources([]Instance{{Use: "forge", Field: "sources[forge]"}})
	if err == nil {
		t.Fatal("BuildSources: want an error")
	}
	if !strings.Contains(err.Error(), "does not implement MatchesRemote") {
		t.Errorf("error %q does not name the missing method", err)
	}
}

func TestBuildSourcesNamesWhatThisBuildHasWhenUseDoesNotResolve(t *testing.T) {
	r := newRegistry(t,
		stubSource{manifest: sourceManifest("github")},
		stubSource{manifest: sourceManifest("jira")},
	)

	_, err := r.BuildSources([]Instance{{ID: "crm", Use: "acme-crm", Field: "sources[crm]"}})
	if err == nil {
		t.Fatal("BuildSources: want an error")
	}
	for _, want := range []string{
		`sources[crm].use names "acme-crm"`,
		"github, jira",
		"declared in plugins:",
		"lore plugin list",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
		t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
	}
}

func TestBuildSourcesRejectsAKindMismatch(t *testing.T) {
	r := newRegistry(t, stubCode{manifest: lore.Manifest{
		Name: "git", Kind: lore.KindCode, APIVersion: lore.APIVersion, Summary: "one local clone",
	}})

	_, err := r.BuildSources([]Instance{{Use: "git", Field: "sources[git]"}})
	if err == nil {
		t.Fatal("BuildSources: want an error")
	}
	if !strings.Contains(err.Error(), "is a code plugin, not a source plugin") {
		t.Errorf("error %q does not name both kinds", err)
	}
}

func TestUnmatchedRemotesAsksTheConnectorsRatherThanNamingAForge(t *testing.T) {
	sources := []lore.Connector{
		matchingConnector{stubConnector: stubConnector{name: "forge"}, remote: "forge:acme/app"},
		stubConnector{name: "notion"},
	}
	clones := []Clone{
		{Path: "/w/app", Remote: "forge:acme/app"},
		{Path: "/w/infra", Remote: "forge:acme/infra"},
		{Path: "/w/scratch"},
	}

	warnings := UnmatchedRemotes(clones, sources)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly the unmatched clone", warnings)
	}
	if !strings.Contains(warnings[0], "/w/infra") {
		t.Errorf("warning %q does not name the unmatched clone", warnings[0])
	}
}
