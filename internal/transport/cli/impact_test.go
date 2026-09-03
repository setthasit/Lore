package cli

import (
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/services"
)

const impactQuestion = "what followed Storage design?"

func TestImpactPrintsWhatFollowedAsATimeline(t *testing.T) {
	rt, impact := mockImpact(t)
	impact.EXPECT().
		ImpactOf(gomock.Any(), services.ImpactRequest{Ref: "PROJ-4521"}).
		Return(timelineBundle(impactQuestion), nil)

	res := run(t, rt, "impact", "PROJ-4521")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !res.released {
		t.Error("the workspace was not released")
	}

	out := res.stdout
	for _, want := range []string{
		impactQuestion,
		"anchor: Storage design\n        https://notion.so/design/storage",
		"2 documents",
		"2025-03-10 Storage design",
		"notion page · arch@example.test · 2025-03-10",
		"https://notion.so/design/storage",
		"2025-03-12 Index on SQLite, not Postgres",
		"github pr · dev@example.test · 2025-03-12 · follow_up",
		"https://github.com/acme/lore/pull/12",
		"chains:",
		"notion:page:design/storage → github:pr:12",
		"gaps:",
		"trail ends at PROJ-4521; no linked follow-up",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n--- output ---\n%s", want, out)
		}
	}

	if strings.Index(out, "2025-03-10 Storage design") > strings.Index(out, "2025-03-12 Index on SQLite") {
		t.Errorf("entries are not in the order the service returned them:\n%s", out)
	}
	if strings.Contains(out, "1. Storage design") {
		t.Errorf("the timeline numbers its entries instead of dating them:\n%s", out)
	}
}

func TestImpactPassesTheQuestionThrough(t *testing.T) {
	rt, impact := mockImpact(t)
	impact.EXPECT().
		ImpactOf(gomock.Any(), services.ImpactRequest{Ref: "PROJ-4521", Question: "did we ever revisit it?"}).
		Return(timelineBundle(impactQuestion), nil)

	res := run(t, rt, "impact", "PROJ-4521", "--question", "did we ever revisit it?")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
}

func TestImpactSendsFreeTextAsTheRef(t *testing.T) {
	rt, impact := mockImpact(t)
	impact.EXPECT().
		ImpactOf(gomock.Any(), services.ImpactRequest{Ref: "the decision to index on sqlite"}).
		Return(timelineBundle(impactQuestion), nil)

	res := run(t, rt, "impact", "the decision to index on sqlite")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
}

func TestImpactReportsAnEmptyTimelineWithItsGap(t *testing.T) {
	rt, impact := mockImpact(t)
	impact.EXPECT().ImpactOf(gomock.Any(), gomock.Any()).Return(&entities.EvidenceBundle{
		Question: impactQuestion,
		Anchor:   documentAnchor(),
		Gaps:     []string{"no follow-up evidence after 2025-03-10"},
	}, nil)

	res := run(t, rt, "impact", "PROJ-4521")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, want %d (an empty timeline is an answer)", res.exitCode, exitOK)
	}
	for _, want := range []string{
		"anchor: Storage design",
		noEvidence,
		"gaps:",
		"no follow-up evidence after 2025-03-10",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output is missing %q\n--- output ---\n%s", want, res.stdout)
		}
	}
}

func TestImpactRawEmitsTheCanonicalBundleJSON(t *testing.T) {
	rt, impact := mockImpact(t)
	bundle := timelineBundle(impactQuestion)
	impact.EXPECT().ImpactOf(gomock.Any(), gomock.Any()).Return(bundle, nil)

	wantBundleJSON(t, run(t, rt, "impact", "PROJ-4521", "--raw"), bundle)
}

func TestImpactExplainsTheTimelineInProse(t *testing.T) {
	rt, impact := mockImpact(t)
	synthesis := mockSynthesis(t, rt)
	bundle := timelineBundle(impactQuestion)
	impact.EXPECT().ImpactOf(gomock.Any(), gomock.Any()).Return(bundle, nil)
	synthesis.EXPECT().Synthesize(gomock.Any(), bundle.Question, bundle).Return(proseAnswer, nil)

	wantProse(t, run(t, rt, "impact", "PROJ-4521", "--explain"))
}

func TestImpactRawOutranksExplain(t *testing.T) {
	rt, impact := mockImpact(t)
	mockSynthesis(t, rt)
	bundle := timelineBundle(impactQuestion)
	impact.EXPECT().ImpactOf(gomock.Any(), gomock.Any()).Return(bundle, nil)

	wantBundleJSON(t, run(t, rt, "impact", "PROJ-4521", "--raw", "--explain"), bundle)
}

func TestImpactWithoutARefIsAUsageError(t *testing.T) {
	rt, _ := mockImpact(t)

	res := run(t, rt, "impact")
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
	}
	if res.released {
		t.Error("the workspace was built for an invocation that could not run")
	}
}

func TestImpactReportsAnUnresolvableRefAsNotFound(t *testing.T) {
	rt, impact := mockImpact(t)
	missing := internalerror.NewNotFoundError(`nothing matches "deadbeef"`, nil)
	impact.EXPECT().ImpactOf(gomock.Any(), gomock.Any()).Return(nil, missing)

	res := run(t, rt, "impact", "deadbeef")
	if res.exitCode != exitNotFound {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitNotFound)
	}
	if !strings.Contains(res.stderr, missing.Error()) {
		t.Errorf("stderr = %q, want it to carry %q", res.stderr, missing)
	}
}

func TestImpactKeepsTheCandidatesOfAnAmbiguousRef(t *testing.T) {
	rt, impact := mockImpact(t)
	ambiguous := internalerror.NewBadRequestError(
		`ref "12" matches 2 documents — retry with one of: `+
			"github:pr:12 (Index on SQLite, not Postgres) https://github.com/acme/lore/pull/12; "+
			"notion:page:design/storage (Storage design) https://notion.so/design/storage", nil)
	impact.EXPECT().ImpactOf(gomock.Any(), services.ImpactRequest{Ref: "12"}).Return(nil, ambiguous)

	res := run(t, rt, "impact", "12")
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitBadRequest)
	}
	for _, want := range []string{
		"retry with one of",
		"github:pr:12 (Index on SQLite, not Postgres) https://github.com/acme/lore/pull/12",
		"notion:page:design/storage (Storage design) https://notion.so/design/storage",
	} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr is missing %q\n--- stderr ---\n%s", want, res.stderr)
		}
	}
}
