package mcp

import (
	"errors"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/services"
)

func impactArgs(refOrQuery string) map[string]any {
	return map[string]any{"ref_or_query": refOrQuery}
}

func TestImpactParsesArguments(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want services.ImpactRequest
	}{
		{
			name: "ref alone",
			args: impactArgs(testRef),
			want: services.ImpactRequest{Ref: testRef},
		},
		{
			name: "ref and question",
			args: map[string]any{"ref_or_query": testRef, "question": testQuestion},
			want: services.ImpactRequest{Ref: testRef, Question: testQuestion},
		},
		{
			name: "the service owns the free text reading",
			args: impactArgs("the decision to drop the read replica"),
			want: services.ImpactRequest{Ref: "the decision to drop the read replica"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			f.impact.EXPECT().
				ImpactOf(gomock.Any(), tt.want).
				Return(&entities.EvidenceBundle{Question: "impact of " + testRef}, nil)

			if res := f.callTool(t, "impact_of", tt.args); res.IsError {
				t.Fatalf("unexpected tool error: %s", errorText(t, res))
			}
		})
	}
}

func TestImpactRequiresRefOrQuery(t *testing.T) {
	f := newToolFixture(t)

	got := errorText(t, f.callTool(t, "impact_of", map[string]any{"question": testQuestion}))

	if !strings.Contains(got, "ref_or_query") {
		t.Errorf("error = %q, want it to name the missing ref_or_query", got)
	}
}

func TestImpactReturnsBundleAsJSON(t *testing.T) {
	f := newToolFixture(t)
	f.impact.EXPECT().
		ImpactOf(gomock.Any(), services.ImpactRequest{Ref: testRef}).
		Return(testBundle(), nil)

	assertResultJSON(t, f.callTool(t, "impact_of", impactArgs(testRef)), testBundleJSON)
}

func TestImpactKeepsAmbiguousRefCandidates(t *testing.T) {
	f := newToolFixture(t)
	f.impact.EXPECT().
		ImpactOf(gomock.Any(), services.ImpactRequest{Ref: testRef}).
		Return(nil, ambiguousRefError(testRef))

	assertKeepsCandidates(t, errorText(t, f.callTool(t, "impact_of", impactArgs(testRef))))
}

func TestImpactMapsServiceErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "not found",
			err:  internalerror.NewNotFoundError("no document matches ref "+testRef, nil),
			want: "not found: no document matches ref " + testRef,
		},
		{
			name: "internal hides the cause",
			err:  internalerror.NewInternalError("walking the provenance graph failed", errors.New(testCause)),
			want: internalErrorMessage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			f.impact.EXPECT().ImpactOf(gomock.Any(), gomock.Any()).Return(nil, tt.err)

			if got := errorText(t, f.callTool(t, "impact_of", impactArgs(testRef))); got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestImpactLogsInternalCauseInsteadOfLeakingIt(t *testing.T) {
	f := newToolFixture(t)
	f.impact.EXPECT().
		ImpactOf(gomock.Any(), gomock.Any()).
		Return(nil, internalerror.NewInternalError("walking the provenance graph failed", errors.New(testCause)))

	if got := errorText(t, f.callTool(t, "impact_of", impactArgs(testRef))); strings.Contains(got, testCause) {
		t.Errorf("error %q leaks the cause", got)
	}
	logged := f.logs.String()
	if !strings.Contains(logged, testCause) {
		t.Errorf("log %q does not record the cause", logged)
	}
	if !strings.Contains(logged, "impact_of failed") {
		t.Errorf("log %q does not name the failing tool", logged)
	}
}
