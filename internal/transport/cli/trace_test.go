package cli

import (
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/services"
)

const traceQuestion = "provenance of Storage design"

func TestTracePrintsTheNeighborhoodAsATimeline(t *testing.T) {
	rt, trace := mockTrace(t)
	trace.EXPECT().
		Trace(gomock.Any(), services.TraceRequest{Ref: "9fceb02"}).
		Return(timelineBundle(traceQuestion), nil)

	res := run(t, rt, "trace", "9fceb02")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !res.released {
		t.Error("the workspace was not released")
	}

	out := res.stdout
	for _, want := range []string{
		traceQuestion,
		"anchor: Storage design\n        https://notion.so/design/storage",
		"2 documents",
		"2025-03-10 Storage design",
		"notion page · arch@example.test · 2025-03-10",
		"https://notion.so/design/storage",
		"2025-03-12 Index on SQLite, not Postgres",
		"github pr · dev@example.test · 2025-03-12 · follow_up",
		"https://github.com/acme/lore/pull/12",
		"sqlite ships everywhere and needs no server",
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

func TestTraceLeadsAnUndatedEntryWithoutADate(t *testing.T) {
	undated := anchorDoc
	undated.CreatedAt = time.Time{}

	rt, trace := mockTrace(t)
	trace.EXPECT().
		Trace(gomock.Any(), services.TraceRequest{Ref: "9fceb02"}).
		Return(&entities.EvidenceBundle{
			Question: traceQuestion,
			Nodes:    []entities.EvidenceNode{{Doc: undated, Role: entities.RoleSeed, Score: 1}},
		}, nil)

	res := run(t, rt, "trace", "9fceb02")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if want := "undated " + undated.Title; !strings.Contains(res.stdout, want) {
		t.Errorf("stdout = %q, want it to lead with %q", res.stdout, want)
	}
}

func TestTraceSendsTheDirectionToTheServiceVerbatim(t *testing.T) {
	for _, direction := range []string{"in", "out", "both"} {
		t.Run(direction, func(t *testing.T) {
			rt, trace := mockTrace(t)
			trace.EXPECT().
				Trace(gomock.Any(), services.TraceRequest{Ref: "PROJ-4521", Direction: direction}).
				Return(timelineBundle(traceQuestion), nil)

			res := run(t, rt, "trace", "PROJ-4521", "--direction", direction)
			if res.exitCode != exitOK {
				t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
			}
		})
	}
}

func TestTraceLeavesAnUnknownDirectionToTheService(t *testing.T) {
	rt, trace := mockTrace(t)
	rejected := internalerror.NewBadRequestError(`direction "sideways" must be one of "in", "out", "both"`, nil)
	trace.EXPECT().
		Trace(gomock.Any(), services.TraceRequest{Ref: "PROJ-4521", Direction: "sideways"}).
		Return(nil, rejected)

	res := run(t, rt, "trace", "PROJ-4521", "--direction", "sideways")
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitBadRequest)
	}
	if !strings.Contains(res.stderr, rejected.Error()) {
		t.Errorf("stderr = %q, want it to carry %q", res.stderr, rejected)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want nothing on failure", res.stdout)
	}
}

func TestTraceRawEmitsTheCanonicalBundleJSON(t *testing.T) {
	rt, trace := mockTrace(t)
	bundle := timelineBundle(traceQuestion)
	trace.EXPECT().Trace(gomock.Any(), gomock.Any()).Return(bundle, nil)

	wantBundleJSON(t, run(t, rt, "trace", "9fceb02", "--raw"), bundle)
}

func TestTraceExplainsTheTimelineInProse(t *testing.T) {
	rt, trace := mockTrace(t)
	synthesis := mockSynthesis(t, rt)
	bundle := timelineBundle(traceQuestion)
	trace.EXPECT().Trace(gomock.Any(), gomock.Any()).Return(bundle, nil)
	synthesis.EXPECT().Synthesize(gomock.Any(), bundle.Question, bundle).Return(proseAnswer, nil)

	wantProse(t, run(t, rt, "trace", "9fceb02", "--explain"))
}

func TestTraceRawOutranksExplain(t *testing.T) {
	rt, trace := mockTrace(t)
	mockSynthesis(t, rt)
	bundle := timelineBundle(traceQuestion)
	trace.EXPECT().Trace(gomock.Any(), gomock.Any()).Return(bundle, nil)

	wantBundleJSON(t, run(t, rt, "trace", "9fceb02", "--raw", "--explain"), bundle)
}

func TestTraceWithoutARefIsAUsageError(t *testing.T) {
	rt, _ := mockTrace(t)

	res := run(t, rt, "trace")
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
	}
	if res.released {
		t.Error("the workspace was built for an invocation that could not run")
	}
}

func TestTraceReportsAnUnresolvableRefAsNotFound(t *testing.T) {
	rt, trace := mockTrace(t)
	missing := internalerror.NewNotFoundError(`ref "deadbeef" matches no document`, nil)
	trace.EXPECT().Trace(gomock.Any(), gomock.Any()).Return(nil, missing)

	res := run(t, rt, "trace", "deadbeef")
	if res.exitCode != exitNotFound {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitNotFound)
	}
	if !strings.Contains(res.stderr, missing.Error()) {
		t.Errorf("stderr = %q, want it to carry %q", res.stderr, missing)
	}
}

func TestTraceKeepsTheCandidatesOfAnAmbiguousRef(t *testing.T) {
	rt, trace := mockTrace(t)
	ambiguous := internalerror.NewBadRequestError(
		`ref "12" matches 2 documents — retry with one of: `+
			"github:pr:12 (Index on SQLite, not Postgres) https://github.com/acme/lore/pull/12; "+
			"notion:page:design/storage (Storage design) https://notion.so/design/storage", nil)
	trace.EXPECT().Trace(gomock.Any(), services.TraceRequest{Ref: "12"}).Return(nil, ambiguous)

	res := run(t, rt, "trace", "12")
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
