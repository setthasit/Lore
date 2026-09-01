package e2e

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"lore/internal/connectors/jira"
	"lore/internal/connectors/notion"
	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/services"
)

const (
	askOnlyFixtures = "askonly"

	// Not credentials; the fixture API only asserts they arrived in Authorization.
	askOnlyNotionToken = "secret_e2e_askonly_notion"
	askOnlyJiraEmail   = "lore-bot@example.invalid"
	askOnlyJiraToken   = "e2e_askonly_jira_token"

	// The Notion fixtures quote Jira browse URLs, which only exist once the
	// fixture server has a port, so they are written against this placeholder.
	askOnlyHost = "https://jira.example"

	askOnlyDocuments = 4

	askOnlyQuestion = "why did we choose option B instead of A?"
	askOnlyEvent    = "incident X"
)

const (
	jiraSource   = "jira"
	notionSource = "notion"

	notionRootPageID     = "a1a1a1a1-0000-4000-8000-000000000001"
	notionDecisionPageID = "d4d4d4d4-0000-4000-8000-000000000004"

	notionDecisionURL = "https://www.notion.so/acme/Adopt-Option-B-d4d4d4d4"

	askOnlyDerivation = `event "incident X" dated 2024-06-03 via jira:ticket:INC-201`
)

var (
	incidentTicketID = entities.NewDocID(jiraSource, entities.DocTypeTicket, "INC-201")
	debateTicketID   = entities.NewDocID(jiraSource, entities.DocTypeTicket, "ARCH-88")
	followUpTicketID = entities.NewDocID(jiraSource, entities.DocTypeTicket, "OPS-410")
	decisionPageID   = entities.NewDocID(notionSource, entities.DocTypePage, notionDecisionPageID)
)

var incidentCreatedAt = time.Date(2024, time.June, 3, 9, 15, 0, 0, time.UTC)

func askOnlyProjects() []string { return []string{"INC", "ARCH", "OPS"} }

const (
	notionSearchPath = "/v1/search"
	notionBlocksPath = "/v1/blocks/"
	notionPagesPath  = "/v1/pages/"

	jiraSearchPath    = "/rest/api/3/search/jql"
	jiraIssuePath     = "/rest/api/3/issue/"
	jiraCommentSuffix = "/comment"
)

// A workspace with no GitHub source and no local clone: every answer below has to
// come out of the two API connectors alone.
func askOnlyWorkspace(ctx context.Context, t *testing.T) *workspace {
	t.Helper()

	api := newFixtureAPI(t, corpusDir(askOnlyFixtures), askOnlyHost)
	api.listen(api.serveAskOnly)

	w := newIndexedWorkspace(t, api, []entities.Connector{
		notion.NewConnector(askOnlyNotionToken, []string{notionRootPageID}, api.server.URL),
		jira.NewConnector(api.server.URL, askOnlyJiraEmail, askOnlyJiraToken, askOnlyProjects()),
	}, nil)
	w.sync(ctx, t, askOnlyFixtures)

	if stats := w.stats(ctx, t); stats.Documents != askOnlyDocuments {
		t.Fatalf("indexed documents = %d, want %d: the corpus is three tickets and one in-scope page",
			stats.Documents, askOnlyDocuments)
	}
	return w
}

func (a *fixtureAPI) serveAskOnly(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		a.expectAuth(r, "Bearer "+askOnlyNotionToken)
		a.serveNotion(w, r)
	case strings.HasPrefix(r.URL.Path, "/rest/"):
		a.expectAuth(r, "Basic bG9yZS1ib3RAZXhhbXBsZS5pbnZhbGlkOmUyZV9hc2tvbmx5X2ppcmFfdG9rZW4=")
		a.serveJira(w, r)
	default:
		a.reject(w, "unexpected request %s %s", r.Method, r.URL.Path)
	}
}

func (a *fixtureAPI) serveNotion(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == notionSearchPath:
		a.write(w, "notion_search.json")
	case strings.HasPrefix(r.URL.Path, notionBlocksPath):
		id := strings.TrimPrefix(r.URL.Path, notionBlocksPath)
		parent, children := strings.CutSuffix(id, "/children")
		if !children {
			a.write(w, "block_"+shortNotionID(id)+".json")
			return
		}
		a.write(w, "blocks_"+shortNotionID(parent)+"_"+bodyPage(r)+".json")
	case strings.HasPrefix(r.URL.Path, notionPagesPath):
		a.write(w, "page_"+shortNotionID(strings.TrimPrefix(r.URL.Path, notionPagesPath))+".json")
	default:
		a.reject(w, "unexpected Notion request %s %s", r.Method, r.URL.Path)
	}
}

func (a *fixtureAPI) serveJira(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == jiraSearchPath {
		a.write(w, "jira_search.json")
		return
	}
	rest, onIssue := strings.CutPrefix(r.URL.Path, jiraIssuePath)
	key, onComments := strings.CutSuffix(rest, jiraCommentSuffix)
	if !onIssue || !onComments {
		a.reject(w, "unexpected Jira request %s %s", r.Method, r.URL.Path)
		return
	}

	a.write(w, "comments_"+key+".json")
}

func (a *fixtureAPI) expectAuth(r *http.Request, want string) {
	if got := r.Header.Get("Authorization"); got != want {
		a.t.Errorf("%s: Authorization = %q, want %q", r.URL.Path, got, want)
	}
}

// Fixture names carry the leading segment of a page id, which is the one form
// shared by the dashed ids in payloads and the undashed ids in configuration.
func shortNotionID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

func bodyPage(r *http.Request) string {
	if cursor := r.URL.Query().Get("start_cursor"); cursor != "" {
		return "2"
	}
	return "1"
}

func askWhyOptionB(ctx context.Context, t *testing.T, w *workspace) *entities.EvidenceBundle {
	t.Helper()

	bundle, err := w.query.FindDecision(ctx, services.FindDecisionRequest{
		Question: askOnlyQuestion,
		Around:   askOnlyEvent,
	})
	if err != nil {
		t.Fatalf("find_decision %q around %q: %v", askOnlyQuestion, askOnlyEvent, err)
	}
	if bundle == nil {
		t.Fatalf("find_decision %q returned no bundle", askOnlyQuestion)
	}
	return bundle
}

func impactOfTheDecisionPage(ctx context.Context, t *testing.T, w *workspace) *entities.EvidenceBundle {
	t.Helper()

	bundle, err := w.impact.ImpactOf(ctx, services.ImpactRequest{Ref: notionDecisionURL})
	if err != nil {
		t.Fatalf("impact_of %q: %v", notionDecisionURL, err)
	}
	return bundle
}

func TestAskOnlyFindDecisionWindowsOnTheJiraIncidentAndChainsToTheNotionPage(t *testing.T) {
	ctx := context.Background()
	w := askOnlyWorkspace(ctx, t)

	bundle := askWhyOptionB(ctx, t, w)

	window := bundle.Anchor.Window
	if window == nil {
		t.Fatalf("event %q resolved to no time window; gaps: %q", askOnlyEvent, bundle.Gaps)
	}
	if window.AnchoredBy != incidentTicketID {
		t.Errorf("window anchored by %s, want the incident ticket %s", window.AnchoredBy, incidentTicketID)
	}
	if window.Derivation != askOnlyDerivation {
		t.Errorf("derivation = %q, want %q", window.Derivation, askOnlyDerivation)
	}
	if !window.From.Equal(incidentCreatedAt.AddDate(0, 0, -30)) || !window.To.Equal(incidentCreatedAt.AddDate(0, 0, 30)) {
		t.Errorf("window %s .. %s, want 30 days either side of %s",
			window.From.Format(time.RFC3339), window.To.Format(time.RFC3339), incidentCreatedAt.Format(time.RFC3339))
	}

	wantChains := []string{
		chainText([]entities.DocID{debateTicketID, incidentTicketID}),
		chainText([]entities.DocID{followUpTicketID, decisionPageID, incidentTicketID}),
	}
	if got := sortedChains(bundle.Chains); !slices.Equal(got, wantChains) {
		t.Errorf("find_decision chains = %q, want %q", got, wantChains)
	}
	assertSpansBothSources(t, "find_decision", bundle.Chains)

	debate, cited := citedNode(bundle, debateTicketID)
	if !cited {
		t.Fatalf("bundle cites %v, not the ticket %s that weighed and rejected option A",
			citedIDs(bundle), debateTicketID)
	}
	if debate.Doc.URL == "" {
		t.Errorf("the rejected-alternative ticket %s carries no URL, so it is not citable", debateTicketID)
	}

	wantCited := []entities.DocID{debateTicketID, incidentTicketID, followUpTicketID, decisionPageID}
	got := citedIDs(bundle)
	slices.Sort(got)
	slices.Sort(wantCited)
	if !slices.Equal(got, wantCited) {
		t.Errorf("find_decision cites %v, want %v", got, wantCited)
	}
	assertBundleContract(t, "find_decision", bundle)
}

func TestAskOnlyImpactOfTheNotionDecisionSurfacesTheLaterJiraTicket(t *testing.T) {
	ctx := context.Background()
	w := askOnlyWorkspace(ctx, t)

	bundle := impactOfTheDecisionPage(ctx, t, w)

	if bundle.Anchor.Doc == nil || bundle.Anchor.Doc.ID != decisionPageID {
		t.Fatalf("impact anchor = %+v, want the decision page %s", bundle.Anchor.Doc, decisionPageID)
	}

	wantTimeline := []entities.DocID{decisionPageID, followUpTicketID}
	if got := citedIDs(bundle); !slices.Equal(got, wantTimeline) {
		t.Fatalf("impact_of timeline = %v, want %v", got, wantTimeline)
	}
	assertChronological(t, "impact_of", bundle.Nodes)

	followUp, _ := citedNode(bundle, followUpTicketID)
	if !followUp.Doc.CreatedAt.After(bundle.Anchor.Doc.CreatedAt) {
		t.Errorf("follow-up %s is dated %s, want after the decision page's %s",
			followUpTicketID, followUp.Doc.CreatedAt.Format(time.RFC3339),
			bundle.Anchor.Doc.CreatedAt.Format(time.RFC3339))
	}
	if followUp.Role != entities.RoleFollowUp {
		t.Errorf("follow-up %s has role %q, want %q: the forward walk reaches it over its reference edge",
			followUpTicketID, followUp.Role, entities.RoleFollowUp)
	}

	wantChains := []string{chainText([]entities.DocID{decisionPageID, followUpTicketID})}
	if got := sortedChains(bundle.Chains); !slices.Equal(got, wantChains) {
		t.Errorf("impact_of chains = %q, want %q", got, wantChains)
	}
	assertSpansBothSources(t, "impact_of", bundle.Chains)
	assertBundleContract(t, "impact_of", bundle)
}

func TestAskOnlyWhyRefusesForWantOfACodeAnchor(t *testing.T) {
	ctx := context.Background()
	w := askOnlyWorkspace(ctx, t)

	bundle, err := w.why.Why(ctx, services.WhyRequest{
		File:      "internal/auth/auth.go",
		LineStart: 10,
		LineEnd:   20,
	})
	if bundle != nil {
		t.Errorf("why returned %+v, want no bundle: an empty bundle would read as no evidence found", bundle)
	}
	if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
		t.Fatalf("why error kind = %s, want %s (error %v)", got, internalerror.KindPrecondition, err)
	}
	if err.Error() != codeAnchorRefusal {
		t.Errorf("why error = %q, want %q", err, codeAnchorRefusal)
	}
}

// The whole point of an ask-only workspace: provenance that no single source holds.
func assertSpansBothSources(t *testing.T, tool string, chains [][]entities.DocID) {
	t.Helper()

	for _, chain := range chains {
		jiraHop := slices.ContainsFunc(chain, func(id entities.DocID) bool {
			return strings.HasPrefix(string(id), jiraSource+":")
		})
		notionHop := slices.ContainsFunc(chain, func(id entities.DocID) bool {
			return strings.HasPrefix(string(id), notionSource+":")
		})
		if jiraHop && notionHop {
			return
		}
	}

	t.Errorf("%s: no chain in %v runs from Jira into Notion", tool, chains)
}

func sortedChains(chains [][]entities.DocID) []string {
	texts := make([]string, len(chains))
	for i, chain := range chains {
		texts[i] = chainText(chain)
	}
	slices.Sort(texts)

	return texts
}

func chainText(chain []entities.DocID) string {
	hops := make([]string, len(chain))
	for i, id := range chain {
		hops[i] = string(id)
	}

	return strings.Join(hops, " -> ")
}
