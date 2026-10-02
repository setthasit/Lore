package cli

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	mock_services "github.com/setthasit/Lore/internal/mocks/services"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/secrets"
	"github.com/setthasit/Lore/internal/services"
	lore "github.com/setthasit/Lore/sdk"
)

const (
	traceQuestion    = "provenance of Storage design"
	traceFocus       = "how fast forward uses a closed form"
	paddedTraceFocus = "  How Fast-Forward uses a closed form \n"
)

var overLimitTraceFocus = strings.Repeat("f", 1001)

type traceRecorder struct {
	mu       sync.Mutex
	requests []services.TraceRequest
}

func recordTraceRequests(trace *mock_services.MockTraceService, bundle *entities.EvidenceBundle) *traceRecorder {
	recorder := &traceRecorder{}
	trace.EXPECT().
		Trace(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, req services.TraceRequest) (*entities.EvidenceBundle, error) {
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			recorder.requests = append(recorder.requests, req)

			return bundle, nil
		}).
		AnyTimes()

	return recorder
}

func (r *traceRecorder) assertOnlyRequest(t *testing.T, want services.TraceRequest) {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) != 1 || r.requests[0] != want {
		t.Errorf("trace requests = %#v, want exactly one, %#v", r.requests, want)
	}
}

func countSyntheses(t *testing.T, rt *Runtime) *atomic.Int32 {
	t.Helper()

	calls := &atomic.Int32{}
	mockSynthesis(t, rt).EXPECT().
		Synthesize(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, string, *entities.EvidenceBundle) (string, error) {
			calls.Add(1)

			return proseAnswer, nil
		}).
		AnyTimes()

	return calls
}

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

func TestTraceMapsFocusOntoTheServiceRequest(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want services.TraceRequest
	}{
		{
			name: "focus",
			args: []string{"trace", "PROJ-4521", "--focus", traceFocus},
			want: services.TraceRequest{Ref: "PROJ-4521", Focus: traceFocus},
		},
		{
			name: "the service owns trimming the focus",
			args: []string{"trace", "PROJ-4521", "--focus", paddedTraceFocus},
			want: services.TraceRequest{Ref: "PROJ-4521", Focus: paddedTraceFocus},
		},
		{
			name: "the service owns the focus length limit",
			args: []string{"trace", "PROJ-4521", "--focus", overLimitTraceFocus},
			want: services.TraceRequest{Ref: "PROJ-4521", Focus: overLimitTraceFocus},
		},
		{
			name: "focus with direction",
			args: []string{"trace", "PROJ-4521", "--direction", "in", "--focus", traceFocus},
			want: services.TraceRequest{Ref: "PROJ-4521", Direction: "in", Focus: traceFocus},
		},
		{
			name: "no focus",
			args: []string{"trace", "PROJ-4521"},
			want: services.TraceRequest{Ref: "PROJ-4521"},
		},
		{
			name: "direction without focus",
			args: []string{"trace", "PROJ-4521", "--direction", "in"},
			want: services.TraceRequest{Ref: "PROJ-4521", Direction: "in"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, trace := mockTrace(t)
			recorder := recordTraceRequests(trace, timelineBundle(traceQuestion))
			syntheses := countSyntheses(t, rt)

			res := run(t, rt, tt.args...)
			if res.exitCode != exitOK {
				t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
			}

			recorder.assertOnlyRequest(t, tt.want)
			if got := syntheses.Load(); got != 0 {
				t.Errorf("synthesis ran %d times, want none without --explain", got)
			}
			for _, want := range []string{
				traceQuestion,
				"2025-03-12 Index on SQLite, not Postgres",
				"postgres with pgvector was the alternative",
			} {
				if !strings.Contains(res.stdout, want) {
					t.Errorf("output is missing %q\n--- output ---\n%s", want, res.stdout)
				}
			}
		})
	}
}

func TestTraceSendsTheFocusAlongsideDirectionAndExplain(t *testing.T) {
	rt, trace := mockTrace(t)
	recorder := recordTraceRequests(trace, timelineBundle(traceQuestion))
	syntheses := countSyntheses(t, rt)

	wantProse(t, run(t, rt, "trace", "PROJ-4521", "--direction", "in", "--focus", traceFocus, "--explain"))
	recorder.assertOnlyRequest(t, services.TraceRequest{Ref: "PROJ-4521", Direction: "in", Focus: traceFocus})
	if got := syntheses.Load(); got != 1 {
		t.Errorf("synthesis ran %d times, want once", got)
	}
}

func TestTraceHelpDocumentsFocus(t *testing.T) {
	flag := newTraceCommand(nil, new(string)).Flags().Lookup("focus")
	if flag == nil {
		t.Fatal("trace declares no --focus flag")
	}
	if flag.Hidden {
		t.Error("--focus is hidden from the help")
	}

	const want = "a question; when set, the anchor excerpt holds only the passages of the document that answer it"
	if flag.Usage != want {
		t.Errorf("usage of --focus = %q, want %q", flag.Usage, want)
	}

	res := run(t, nil, "trace", "--help")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	focusLine := regexp.MustCompile(`(?m)--focus string\s+` + regexp.QuoteMeta(want) + `$`)
	if !focusLine.MatchString(res.stdout) {
		t.Errorf("help does not show --focus as %q\n--- help ---\n%s", want, res.stdout)
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

func TestTraceTimelineScrubsASecretItRendersInert(t *testing.T) {
	const secret = "fake\x01tok\"en-value"
	sink := &secrets.Sink{}
	sink.Record("LORE_FORGE_TOKEN", secret)
	rt, trace := mockTrace(t)
	bundle := timelineBundle(traceQuestion)
	bundle.Gaps = []string{"forge rejected " + secret}
	trace.EXPECT().Trace(gomock.Any(), gomock.Any()).Return(bundle, nil)

	res := runOn(t, registry.New(lore.Host{}, sink), rt, "", "trace", "9fceb02")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if want := "  forge rejected " + secrets.Placeholder + "\n"; !strings.Contains(res.stdout, want) {
		t.Errorf("stdout is missing %q\n--- stdout ---\n%s", want, res.stdout)
	}
}
