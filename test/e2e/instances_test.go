package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/fx"

	"github.com/setthasit/Lore/app"
	"github.com/setthasit/Lore/internal/di"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/repositories"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/plugins/sources/jira"
	"github.com/setthasit/Lore/sdk"
)

const (
	instanceFixtures = "instances"

	acmeInstance   = "jira-acme"
	legacyInstance = "jira-legacy"

	acmeSitePath   = "/acme"
	legacySitePath = "/legacy"

	acmeTicket   = "ACME-1"
	legacyTicket = "OLD-7"

	acmeCommentID   = "10101"
	legacyCommentID = "20202"

	instanceJiraEmail = "lore-bot@example.invalid"
	instanceJiraToken = "e2e_instances_jira_token"

	instanceEmailEnv = "LORE_E2E_JIRA_EMAIL"
	instanceTokenEnv = "LORE_E2E_JIRA_TOKEN"

	instanceDocuments    = 4
	perInstanceDocuments = 2

	noFailingInstance = ""

	// No fixture here quotes a browse URL, so fixtureAPI.write looks for this placeholder and never finds it.
	instanceFixtureHost = "https://jira.example"

	instanceSearchCall   = "search"
	instanceCommentsCall = "comments"

	stubProviderName      = "e2e-stub"
	stubEmbedderModelName = "bag-of-words"

	instanceWorkspaceIndex = "instances.db"
)

var instanceAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte(instanceJiraEmail+":"+instanceJiraToken))

const instanceConfig = `workspace: lore-e2e-instances
index_path: %[1]s
sources:
  - id: jira-acme
    use: jira
    with:
      base_url: %[2]s/acme
      projects: [ACME]
      email_env: LORE_E2E_JIRA_EMAIL
      token_env: LORE_E2E_JIRA_TOKEN
  - id: jira-legacy
    use: jira
    with:
      base_url: %[2]s/legacy
      projects: [OLD]
      email_env: LORE_E2E_JIRA_EMAIL
      token_env: LORE_E2E_JIRA_TOKEN
embedder:
  provider: e2e-stub
  model: bag-of-words
`

type site struct {
	instance string
	path     string
	search   string
	ticket   string
	comment  string
}

var instanceSites = []site{
	{instance: acmeInstance, path: acmeSitePath, search: "search_acme.json", ticket: acmeTicket, comment: acmeCommentID},
	{instance: legacyInstance, path: legacySitePath, search: "search_legacy.json", ticket: legacyTicket, comment: legacyCommentID},
}

func siteOf(t *testing.T, instance string) site {
	t.Helper()

	for _, s := range instanceSites {
		if s.instance == instance {
			return s
		}
	}
	t.Fatalf("no fixture site for instance %q", instance)
	return site{}
}

func (s site) docIDs() []lore.DocID {
	return []lore.DocID{
		lore.NewDocID(s.instance, lore.DocTypeTicket, s.ticket),
		lore.NewDocID(s.instance, lore.DocTypeTicketComment, s.ticket+"#"+s.comment),
	}
}

type stubEmbedderPlugin struct{}

var _ lore.ProviderPlugin = stubEmbedderPlugin{}

func (stubEmbedderPlugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:          stubProviderName,
		Kind:          lore.KindProvider,
		APIVersion:    lore.APIVersion,
		Summary:       "in-process embedder for the end-to-end suite",
		Capabilities:  lore.Capabilities{Embed: true},
		DefaultModels: map[lore.Capability]string{lore.CapabilityEmbed: stubEmbedderModelName},
	}
}

func (stubEmbedderPlugin) NewProvider(c lore.ProviderConfig) (lore.Provider, error) {
	if c.Capability != lore.CapabilityEmbed {
		return nil, fmt.Errorf("%s serves embeddings only, not %s", stubProviderName, c.Capability)
	}
	return fakeEmbedder{}, nil
}

type instanceWorkspace struct {
	api    *fixtureAPI
	store  repositories.IndexStore
	round  services.SyncOrchestrator
	status services.StatusService
}

// failing is the instance whose site answers every request with a hard error; empty leaves both healthy.
func newInstanceWorkspace(t *testing.T, failing string) *instanceWorkspace {
	t.Helper()

	t.Setenv(instanceEmailEnv, instanceJiraEmail)
	t.Setenv(instanceTokenEnv, instanceJiraToken)

	api := newFixtureAPI(t, corpusDir(instanceFixtures), instanceFixtureHost)
	api.listen(func(w http.ResponseWriter, r *http.Request) { api.serveInstanceSites(w, r, failing) })

	reg := registry.New(app.Host())
	if err := reg.Register(jira.Plugin(), stubEmbedderPlugin{}); err != nil {
		t.Fatalf("register the plugins this workspace runs on: %v", err)
	}

	w := &instanceWorkspace{api: api}
	graph := fx.New(
		fx.NopLogger,
		di.Workspace(writeInstanceConfig(t, api.server.URL), reg),
		fx.Populate(&w.store, &w.round, &w.status),
	)
	if err := graph.Err(); err != nil {
		t.Fatalf("build the workspace graph: %v", err)
	}
	if err := graph.Start(context.Background()); err != nil {
		t.Fatalf("start the workspace: %v", err)
	}
	t.Cleanup(func() {
		if err := graph.Stop(context.Background()); err != nil {
			t.Errorf("stop the workspace: %v", err)
		}
	})

	return w
}

func writeInstanceConfig(t *testing.T, baseURL string) string {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "lore.yaml")
	body := fmt.Sprintf(instanceConfig, strconv.Quote(filepath.Join(dir, instanceWorkspaceIndex)), baseURL)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", configPath, err)
	}

	return configPath
}

// A failing site answers with a status the Jira client does not retry, so the round reaches the other instance instead of spending its retry budget.
func (a *fixtureAPI) serveInstanceSites(w http.ResponseWriter, r *http.Request, failing string) {
	for _, s := range instanceSites {
		rest, ok := strings.CutPrefix(r.URL.Path, s.path)
		if !ok {
			continue
		}
		if s.instance == failing {
			http.Error(w, `{"errorMessages":["the `+s.instance+` site is gone"]}`, http.StatusGone)
			return
		}

		a.expectAuth(r, instanceAuth)
		a.serveInstanceSite(w, r, s, rest)
		return
	}

	a.reject(w, "unexpected request %s %s: no instance site claims it", r.Method, r.URL.Path)
}

func (a *fixtureAPI) serveInstanceSite(w http.ResponseWriter, r *http.Request, s site, path string) {
	if path == jiraSearchPath {
		a.record(s.call(instanceSearchCall))
		a.write(w, s.search)
		return
	}

	rest, onIssue := strings.CutPrefix(path, jiraIssuePath)
	key, onComments := strings.CutSuffix(rest, jiraCommentSuffix)
	if !onIssue || !onComments {
		a.reject(w, "unexpected request %s %s on the %s site", r.Method, r.URL.Path, s.instance)
		return
	}

	a.record(s.call(instanceCommentsCall))
	a.write(w, "comments_"+key+".json")
}

func (s site) call(op string) string { return s.instance + " " + op }

func (w *instanceWorkspace) sync(ctx context.Context, t *testing.T, opts services.SyncOptions) services.SyncResult {
	t.Helper()

	result, err := w.round.Sync(ctx, opts)
	if err != nil {
		t.Fatalf("sync %+v: %v", opts, err)
	}

	return result
}

func (w *instanceWorkspace) stats(ctx context.Context, t *testing.T) entities.IndexStats {
	t.Helper()

	stats, err := w.status.Status(ctx)
	if err != nil {
		t.Fatalf("read the index's state: %v", err)
	}

	return stats
}

func (w *instanceWorkspace) cursor(ctx context.Context, t *testing.T, instance string) lore.Cursor {
	t.Helper()

	cursor, err := w.store.Cursor(ctx, instance)
	if err != nil {
		t.Fatalf("read the cursor of %s: %v", instance, err)
	}

	return cursor
}

func (w *instanceWorkspace) assertOwns(ctx context.Context, t *testing.T, instance string) {
	t.Helper()

	s := siteOf(t, instance)
	want := s.docIDs()
	metas, err := w.store.DocumentsByID(ctx, want)
	if err != nil {
		t.Fatalf("read the documents of %s: %v", instance, err)
	}
	if len(metas) != len(want) {
		t.Fatalf("the index holds %d of the %d documents %s synced (%v)", len(metas), len(want), instance, want)
	}

	for _, meta := range metas {
		if meta.Source != instance {
			t.Errorf("%s reports source %q, want the instance id %q", meta.ID, meta.Source, instance)
		}
		if !strings.HasPrefix(string(meta.ID), instance+":") {
			t.Errorf("document id %s is not prefixed with the instance id %q", meta.ID, instance)
		}
	}
}

func (w *instanceWorkspace) assertNotIndexed(ctx context.Context, t *testing.T, ids ...lore.DocID) {
	t.Helper()

	metas, err := w.store.DocumentsByID(ctx, ids)
	if err != nil {
		t.Fatalf("read %v: %v", ids, err)
	}
	for _, meta := range metas {
		t.Errorf("the index holds %s, want nothing under that id", meta.ID)
	}
}

func cursorKeys(stats entities.IndexStats) []string {
	keys := make([]string, len(stats.Cursors))
	for i, cursor := range stats.Cursors {
		keys[i] = cursor.Connector
	}

	return keys
}

func TestTwoJiraInstancesSyncUnderTheirOwnIdentities(t *testing.T) {
	ctx := context.Background()
	w := newInstanceWorkspace(t, noFailingInstance)

	if result := w.sync(ctx, t, services.SyncOptions{}); len(result.Failures) != 0 {
		t.Fatalf("a round over two healthy sites reported failures %+v", result.Failures)
	}

	stats := w.stats(ctx, t)
	if stats.Documents != instanceDocuments {
		t.Fatalf("indexed documents = %d, want %d: one ticket and one comment per instance",
			stats.Documents, instanceDocuments)
	}

	w.assertOwns(ctx, t, acmeInstance)
	w.assertOwns(ctx, t, legacyInstance)

	w.assertNotIndexed(ctx, t,
		lore.NewDocID(acmeInstance, lore.DocTypeTicket, legacyTicket),
		lore.NewDocID(legacyInstance, lore.DocTypeTicket, acmeTicket),
		lore.NewDocID("jira", lore.DocTypeTicket, acmeTicket),
		lore.NewDocID("jira", lore.DocTypeTicket, legacyTicket),
	)

	if keys := cursorKeys(stats); !slices.Equal(keys, []string{acmeInstance, legacyInstance}) {
		t.Fatalf("checkpointed cursor keys = %v, want one per instance", keys)
	}

	acme, legacy := w.cursor(ctx, t, acmeInstance), w.cursor(ctx, t, legacyInstance)
	if len(acme) == 0 || len(legacy) == 0 {
		t.Fatalf("cursors = %v and %v, want both instances checkpointed", acme, legacy)
	}
	// The two sites sit at different watermarks, so equal cursors would mean one shared position.
	if maps.Equal(acme, legacy) {
		t.Errorf("both instances resume from %v, want each site's own position", acme)
	}
}

func TestSyncingOneJiraInstanceLeavesTheOtherUntouched(t *testing.T) {
	ctx := context.Background()
	w := newInstanceWorkspace(t, noFailingInstance)

	if result := w.sync(ctx, t, services.SyncOptions{Source: legacyInstance}); len(result.Failures) != 0 {
		t.Fatalf("a round over one healthy site reported failures %+v", result.Failures)
	}

	stats := w.stats(ctx, t)
	if stats.Documents != perInstanceDocuments {
		t.Fatalf("indexed documents = %d, want the %d of %s alone", stats.Documents, perInstanceDocuments, legacyInstance)
	}

	w.assertOwns(ctx, t, legacyInstance)
	w.assertNotIndexed(ctx, t, siteOf(t, acmeInstance).docIDs()...)

	if keys := cursorKeys(stats); !slices.Equal(keys, []string{legacyInstance}) {
		t.Errorf("checkpointed cursor keys = %v, want %s alone", keys, legacyInstance)
	}

	acme := siteOf(t, acmeInstance)
	for _, op := range []string{instanceSearchCall, instanceCommentsCall} {
		if calls := w.api.callCount(acme.call(op)); calls != 0 {
			t.Errorf("--source=%s asked the %s site for %s %d times, want it left alone",
				legacyInstance, acmeInstance, op, calls)
		}
	}
}

// The broken instance is declared first, so a round that stopped at it would leave the healthy one with nothing.
func TestAFailingJiraInstanceDoesNotStopTheHealthyOne(t *testing.T) {
	ctx := context.Background()
	w := newInstanceWorkspace(t, acmeInstance)

	result, err := w.round.Sync(ctx, services.SyncOptions{})
	if err != nil {
		t.Fatalf("Sync() = %v, want no error: one broken instance must not fail the round", err)
	}

	if len(result.Failures) != 1 || result.Failures[0].Instance != acmeInstance {
		t.Fatalf("failures = %+v, want exactly one naming %s", result.Failures, acmeInstance)
	}
	failure := result.Failures[0]
	if failure.Err == nil {
		t.Fatalf("failure of %s carries no error", failure.Instance)
	}
	if !strings.Contains(failure.Err.Error(), acmeSitePath) {
		t.Errorf("failure of %s = %v, want it to name the site that broke (%s)", failure.Instance, failure.Err, acmeSitePath)
	}

	stats := w.stats(ctx, t)
	if stats.Documents != perInstanceDocuments {
		t.Fatalf("indexed documents = %d, want the %d of %s: its round follows the broken instance's",
			stats.Documents, perInstanceDocuments, legacyInstance)
	}

	w.assertOwns(ctx, t, legacyInstance)
	w.assertNotIndexed(ctx, t, siteOf(t, acmeInstance).docIDs()...)

	if keys := cursorKeys(stats); !slices.Equal(keys, []string{legacyInstance}) {
		t.Errorf("checkpointed cursor keys = %v, want %s alone: the broken instance advanced nothing",
			keys, legacyInstance)
	}
	if cursor := w.cursor(ctx, t, acmeInstance); len(cursor) != 0 {
		t.Errorf("the broken instance checkpointed %v, want its position untouched", cursor)
	}
}
