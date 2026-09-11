package e2e

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport/mcp"
	"github.com/setthasit/Lore/sdk"
)

const (
	provenanceFixtures = "provenance"

	incidentPhrase   = "quorum drift brownout"
	decisionQuestion = "why does ingest serialize its writes through one writer"
	decisionRef      = fixtureRepo + "#64"

	incidentDate = "2024-03-12"

	githubSource = "github"
)

var incidentAt = time.Date(2024, time.March, 12, 9, 15, 0, 0, time.UTC)

var (
	incidentDocID = lore.NewDocID(githubSource, lore.DocTypeIssue, fixtureRepo+"/issues/61")
	debateDocID   = lore.NewDocID(githubSource, lore.DocTypeIssue, fixtureRepo+"/issues/62")
	decisionDocID = lore.NewDocID(githubSource, lore.DocTypePR, fixtureRepo+"/pull/64")
	revertDocID   = lore.NewDocID(githubSource, lore.DocTypePR, fixtureRepo+"/pull/67")
	unlinkedDocID = lore.NewDocID(githubSource, lore.DocTypeIssue, fixtureRepo+"/issues/69")
)

// The chunker splits the decision body, so no retrieval excerpt holds both.
const (
	decisionOpening = "Ingest writes now go through a single writer goroutine"
	decisionRollout = "the per-worker connections are deleted in the same change"
)

func provenanceWorkspace(ctx context.Context, t *testing.T) *workspace {
	t.Helper()

	w := newWorkspace(t, provenanceFixtures)
	w.sync(ctx, t, provenanceFixtures)
	return w
}

func askAroundTheIncident(ctx context.Context, t *testing.T, w *workspace) *entities.EvidenceBundle {
	t.Helper()

	bundle, err := w.query.FindDecision(ctx, services.FindDecisionRequest{
		Question: decisionQuestion,
		Around:   incidentPhrase,
	})
	if err != nil {
		t.Fatalf("find_decision %q around %q: %v", decisionQuestion, incidentPhrase, err)
	}
	return bundle
}

func traceTheDecision(ctx context.Context, t *testing.T, w *workspace) *entities.EvidenceBundle {
	t.Helper()

	bundle, err := w.trace.Trace(ctx, services.TraceRequest{Ref: decisionRef})
	if err != nil {
		t.Fatalf("trace %q: %v", decisionRef, err)
	}
	return bundle
}

func impactOfTheDecision(ctx context.Context, t *testing.T, w *workspace) *entities.EvidenceBundle {
	t.Helper()

	bundle, err := w.impact.ImpactOf(ctx, services.ImpactRequest{Ref: decisionRef})
	if err != nil {
		t.Fatalf("impact_of %q: %v", decisionRef, err)
	}
	return bundle
}

func citedNode(bundle *entities.EvidenceBundle, id lore.DocID) (entities.EvidenceNode, bool) {
	for _, node := range bundle.Nodes {
		if node.Doc.ID == id {
			return node, true
		}
	}
	return entities.EvidenceNode{}, false
}

func TestFindDecisionAroundAnEventWindowsOnTheIncidentThatDatedIt(t *testing.T) {
	ctx := context.Background()
	w := provenanceWorkspace(ctx, t)

	bundle := askAroundTheIncident(ctx, t, w)

	const wantKind = entities.AnchorQuery | entities.AnchorTimeWindow
	if bundle.Anchor.Kind != wantKind {
		t.Errorf("anchor kind = %d, want AnchorQuery|AnchorTimeWindow (%d); gaps: %q",
			bundle.Anchor.Kind, wantKind, bundle.Gaps)
	}

	window := bundle.Anchor.Window
	if window == nil {
		t.Fatalf("event %q resolved to no time window; gaps: %q", incidentPhrase, bundle.Gaps)
	}
	if window.AnchoredBy != incidentDocID {
		t.Errorf("window anchored by %s, want the incident issue %s", window.AnchoredBy, incidentDocID)
	}
	if !strings.Contains(window.Derivation, incidentDate) {
		t.Errorf("derivation %q does not name the incident's date %s", window.Derivation, incidentDate)
	}
	if !window.From.Before(incidentAt) || !window.To.After(incidentAt) {
		t.Errorf("window %s .. %s does not bracket the incident at %s",
			window.From.Format(time.RFC3339), window.To.Format(time.RFC3339), incidentAt.Format(time.RFC3339))
	}

	if len(bundle.Chains) == 0 {
		t.Errorf("bundle carries no chain, though the decision closes the incident and cites the debate")
	}
	assertBundleContract(t, "find_decision", bundle)
}

func TestFindDecisionReportsTheUnreferencedIssueAsStandingAlone(t *testing.T) {
	ctx := context.Background()
	w := provenanceWorkspace(ctx, t)

	bundle := askAroundTheIncident(ctx, t, w)

	unlinked, cited := citedNode(bundle, unlinkedDocID)
	if !cited {
		t.Fatalf("bundle cites %v, not the unreferenced issue %s", citedIDs(bundle), unlinkedDocID)
	}

	want := unlinked.Doc.Title + " (" + string(unlinked.Doc.ID) + ") stands alone; no linked discussion"
	if !slices.Contains(bundle.Gaps, want) {
		t.Errorf("gaps %q do not report %q", bundle.Gaps, want)
	}
}

func TestImpactOfTheDecisionTimelinesOnlyWhatFollowedIt(t *testing.T) {
	ctx := context.Background()
	w := provenanceWorkspace(ctx, t)

	bundle := impactOfTheDecision(ctx, t, w)

	if bundle.Anchor.Doc == nil || bundle.Anchor.Doc.ID != decisionDocID {
		t.Fatalf("impact anchor = %+v, want the decision pull request %s", bundle.Anchor.Doc, decisionDocID)
	}
	if len(bundle.Nodes) == 0 {
		t.Fatalf("impact_of %q cites nothing", decisionRef)
	}
	if first := bundle.Nodes[0].Doc.ID; first != decisionDocID {
		t.Errorf("timeline opens on %s, want the anchor %s", first, decisionDocID)
	}
	assertChronological(t, "impact_of", bundle.Nodes)

	revert, cited := citedNode(bundle, revertDocID)
	if !cited {
		t.Fatalf("timeline %v omits the revert %s that references the decision", citedIDs(bundle), revertDocID)
	}
	if revert.Role != entities.RoleFollowUp {
		t.Errorf("revert %s has role %q, want %q: the forward walk reaches it over its reference edge",
			revertDocID, revert.Role, entities.RoleFollowUp)
	}
	if _, cited := citedNode(bundle, debateDocID); cited {
		t.Errorf("timeline cites %s, which predates the decision it links", debateDocID)
	}
	if len(bundle.Chains) == 0 {
		t.Errorf("impact bundle carries no chain, though the revert links back to the anchor")
	}
	assertBundleContract(t, "impact_of", bundle)
}

func TestTraceResolvesTheDecisionToOneDocumentAndItsWholeBody(t *testing.T) {
	ctx := context.Background()
	w := provenanceWorkspace(ctx, t)

	bundle := traceTheDecision(ctx, t, w)

	if bundle.Anchor.Doc == nil || bundle.Anchor.Doc.ID != decisionDocID {
		t.Fatalf("trace %q anchored on %+v, want the decision pull request %s",
			decisionRef, bundle.Anchor.Doc, decisionDocID)
	}
	resolved := 0
	for _, node := range bundle.Nodes {
		if node.Role == entities.RoleSeed {
			resolved++
		}
	}
	if resolved != 1 {
		t.Errorf("trace %q resolved %d documents, want exactly one", decisionRef, resolved)
	}

	anchor, cited := citedNode(bundle, decisionDocID)
	if !cited {
		t.Fatalf("trace %q cites %v, not its own anchor %s", decisionRef, citedIDs(bundle), decisionDocID)
	}
	for _, phrase := range []string{decisionOpening, decisionRollout} {
		if !strings.Contains(anchor.Excerpt, phrase) {
			t.Errorf("anchor excerpt omits %q, so it is not the whole body: %q", phrase, anchor.Excerpt)
		}
	}

	if _, cited := citedNode(bundle, incidentDocID); !cited {
		t.Errorf("neighbourhood %v omits the incident %s the decision closes", citedIDs(bundle), incidentDocID)
	}
	assertChronological(t, "trace", bundle.Nodes)
	assertBundleContract(t, "trace", bundle)
}

func TestEveryProvenanceToolAnswersWithoutALocalRepository(t *testing.T) {
	ctx := context.Background()
	w := provenanceWorkspace(ctx, t)

	bundles := map[string]*entities.EvidenceBundle{
		"find_decision": askAroundTheIncident(ctx, t, w),
		"trace":         traceTheDecision(ctx, t, w),
		"impact_of":     impactOfTheDecision(ctx, t, w),
	}
	for tool, bundle := range bundles {
		if len(bundle.Nodes) == 0 {
			t.Errorf("%s cites nothing", tool)
		}
		if bundle.Anchor.Kind&entities.AnchorCodeSpan != 0 || bundle.Anchor.Code != nil {
			t.Errorf("%s anchored on code, though the workspace registers no clone: %+v", tool, bundle.Anchor)
		}
		for _, node := range bundle.Nodes {
			if node.Doc.Source != githubSource {
				t.Errorf("%s cites %s from %q, want %q: the workspace configures one API connector and no clone",
					tool, node.Doc.ID, node.Doc.Source, githubSource)
			}
		}
	}
}

func assertChronological(t *testing.T, tool string, nodes []entities.EvidenceNode) {
	t.Helper()

	for i := 1; i < len(nodes); i++ {
		if node, prev := nodes[i], nodes[i-1]; node.Doc.CreatedAt.Before(prev.Doc.CreatedAt) {
			t.Errorf("%s: node %d (%s at %s) precedes node %d (%s at %s)",
				tool, i, node.Doc.ID, node.Doc.CreatedAt.Format(time.RFC3339),
				i-1, prev.Doc.ID, prev.Doc.CreatedAt.Format(time.RFC3339))
		}
	}
}

func assertBundleContract(t *testing.T, tool string, bundle *entities.EvidenceBundle) {
	t.Helper()

	cited := make(map[lore.DocID]bool, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		if node.Doc.URL == "" {
			t.Errorf("%s: node %s carries no URL, so it is not citable evidence", tool, node.Doc.ID)
		}
		if cited[node.Doc.ID] {
			t.Errorf("%s: node %s is cited twice", tool, node.Doc.ID)
		}
		cited[node.Doc.ID] = true
	}
	for _, chain := range bundle.Chains {
		for _, id := range chain {
			if !cited[id] {
				t.Errorf("%s: chain %v names %s, which the bundle does not cite", tool, chain, id)
			}
		}
	}

	encoded, err := mcp.EncodeBundle(bundle)
	if err != nil {
		t.Fatalf("%s: encode the bundle: %v", tool, err)
	}
	wire := string(encoded)
	for _, want := range anchorTexts(bundle.Anchor) {
		if !strings.Contains(wire, jsonText(t, want)) {
			t.Errorf("%s: encoded bundle drops the anchor's %q", tool, want)
		}
	}
	for _, node := range bundle.Nodes {
		if !strings.Contains(wire, jsonText(t, node.Doc.URL)) {
			t.Errorf("%s: encoded bundle drops the URL of %s", tool, node.Doc.ID)
		}
	}
}

func anchorTexts(a entities.Anchor) []string {
	var texts []string
	if a.Query != "" {
		texts = append(texts, a.Query)
	}
	if a.Code != nil {
		texts = append(texts, a.Code.Repo, a.Code.File)
		texts = append(texts, a.Code.BlamedSHAs...)
	}
	if a.Doc != nil {
		texts = append(texts, string(a.Doc.ID), a.Doc.URL)
	}
	if a.Window != nil {
		texts = append(texts, a.Window.Derivation)
	}
	return texts
}

// Quoted, so a match is a whole JSON value rather than a fragment of a longer one.
func jsonText(t *testing.T, s string) string {
	t.Helper()

	quoted, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("encode %q as JSON text: %v", s, err)
	}
	return string(quoted)
}

func citedIDs(bundle *entities.EvidenceBundle) []lore.DocID {
	ids := make([]lore.DocID, len(bundle.Nodes))
	for i, node := range bundle.Nodes {
		ids[i] = node.Doc.ID
	}
	return ids
}
