package mcp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	mock_services "github.com/setthasit/Lore/internal/mocks/services"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport"
)

const (
	testRef          = "abc1234"
	testCause        = "dial 10.1.2.3:5432: connection refused"
	traceFocus       = "how fast forward uses a closed form"
	paddedTraceFocus = "  How Fast-Forward uses a closed form \n"
)

var overLimitTraceFocus = strings.Repeat("f", 1001)

type refCandidate struct {
	id    string
	title string
	url   string
}

var ambiguousCandidates = []refCandidate{
	{"github:commit:acme/lore/commit/aaa1111", "cap the pool at 20", "https://example.test/aaa1111"},
	{"github:pr:acme/lore/pull/42", "switch to pgbouncer", "https://example.test/pull/42"},
	{"jira:ticket:PROJ-4521", "connection storm postmortem", "https://example.test/PROJ-4521"},
}

func ambiguousRefError(ref string) error {
	listed := make([]string, len(ambiguousCandidates))
	for i, candidate := range ambiguousCandidates {
		listed[i] = fmt.Sprintf("%s (%s) %s", candidate.id, candidate.title, candidate.url)
	}

	return internalerror.NewBadRequestError(fmt.Sprintf("ref %q matches %d documents — retry with one of: %s",
		ref, len(ambiguousCandidates), strings.Join(listed, "; ")), nil)
}

func assertKeepsCandidates(t *testing.T, got string) {
	t.Helper()

	if !strings.HasPrefix(got, "invalid argument: ") {
		t.Errorf("error = %q, want it reported as an invalid argument", got)
	}
	for _, candidate := range ambiguousCandidates {
		for _, part := range []string{candidate.id, candidate.title, candidate.url} {
			if !strings.Contains(got, part) {
				t.Errorf("error = %q, want it to keep candidate detail %q", got, part)
			}
		}
	}
}

func traceArgs(ref string) map[string]any {
	return map[string]any{"ref": ref}
}

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

func fieldType(t *testing.T, properties map[string]any, field string) string {
	t.Helper()

	property, ok := properties[field].(map[string]any)
	if !ok {
		t.Fatalf("property %q is %T, want map[string]any", field, properties[field])
	}
	kind, ok := property["type"].(string)
	if !ok {
		t.Fatalf("type of %q is %T, want string", field, property["type"])
	}

	return kind
}

func TestTraceParsesArguments(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want services.TraceRequest
	}{
		{
			name: "ref alone",
			args: traceArgs(testRef),
			want: services.TraceRequest{Ref: testRef},
		},
		{
			name: "direction",
			args: map[string]any{"ref": testRef, "direction": "in"},
			want: services.TraceRequest{Ref: testRef, Direction: "in"},
		},
		{
			name: "depth",
			args: map[string]any{"ref": testRef, "depth": 1},
			want: services.TraceRequest{Ref: testRef, Depth: 1},
		},
		{
			name: "direction and depth",
			args: map[string]any{"ref": testRef, "direction": "out", "depth": 3},
			want: services.TraceRequest{Ref: testRef, Direction: "out", Depth: 3},
		},
		{
			name: "the service owns the direction vocabulary",
			args: map[string]any{"ref": testRef, "direction": "sideways"},
			want: services.TraceRequest{Ref: testRef, Direction: "sideways"},
		},
		{
			name: "the service owns the depth policy",
			args: map[string]any{"ref": testRef, "depth": -1},
			want: services.TraceRequest{Ref: testRef, Depth: -1},
		},
		{
			name: "focus",
			args: map[string]any{"ref": testRef, "focus": traceFocus},
			want: services.TraceRequest{Ref: testRef, Focus: traceFocus},
		},
		{
			name: "the service owns trimming the focus",
			args: map[string]any{"ref": testRef, "focus": paddedTraceFocus},
			want: services.TraceRequest{Ref: testRef, Focus: paddedTraceFocus},
		},
		{
			name: "the service owns the focus length limit",
			args: map[string]any{"ref": testRef, "focus": overLimitTraceFocus},
			want: services.TraceRequest{Ref: testRef, Focus: overLimitTraceFocus},
		},
		{
			name: "focus with direction",
			args: map[string]any{"ref": testRef, "direction": "in", "focus": traceFocus},
			want: services.TraceRequest{Ref: testRef, Direction: "in", Focus: traceFocus},
		},
		{
			name: "focus with depth",
			args: map[string]any{"ref": testRef, "depth": 3, "focus": traceFocus},
			want: services.TraceRequest{Ref: testRef, Depth: 3, Focus: traceFocus},
		},
		{
			name: "focus with direction and depth",
			args: map[string]any{"ref": testRef, "direction": "in", "depth": 3, "focus": traceFocus},
			want: services.TraceRequest{Ref: testRef, Direction: "in", Depth: 3, Focus: traceFocus},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			recorder := recordTraceRequests(f.trace, testBundle())

			res := f.callTool(t, "trace", tt.args)

			recorder.assertOnlyRequest(t, tt.want)
			assertResultJSON(t, res, testBundleJSON)
		})
	}
}

func TestTraceRequiresRef(t *testing.T) {
	f := newToolFixture(t)

	got := errorText(t, f.callTool(t, "trace", map[string]any{"direction": "in"}))

	if !strings.Contains(got, "ref") {
		t.Errorf("error = %q, want it to name the missing ref", got)
	}
}

func TestTraceToolDeclaration(t *testing.T) {
	f := newToolFixture(t)

	tool := f.declaration(t, "trace")

	for _, stale := range []string{"its own full text", "whole body"} {
		if strings.Contains(tool.Description, stale) {
			t.Errorf("description = %q, want it to stop promising %q", tool.Description, stale)
		}
	}
	for _, phrase := range []string{"up to 8,000 characters", "over 8,000 characters", "first 8,000", "Pass focus"} {
		if !strings.Contains(tool.Description, phrase) {
			t.Errorf("description = %q, want it to carry %q", tool.Description, phrase)
		}
	}
	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema is %T, want map[string]any", tool.InputSchema)
	}
	if got := schema["required"]; !reflect.DeepEqual(got, []any{"ref"}) {
		t.Errorf("required = %v, want [ref]", got)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties is %T, want map[string]any", schema["properties"])
	}
	if got := fieldType(t, properties, "focus"); got != "string" {
		t.Errorf("type of focus = %q, want string", got)
	}
	const want = "a question; when set, the anchor excerpt holds only the passages of the document that answer it"
	if got := fieldDescription(t, properties, "focus"); got != want {
		t.Errorf("description of focus = %q, want %q", got, want)
	}
}

func TestTraceReturnsBundleAsJSON(t *testing.T) {
	f := newToolFixture(t)
	f.trace.EXPECT().
		Trace(gomock.Any(), services.TraceRequest{Ref: testRef}).
		Return(testBundle(), nil)

	assertResultJSON(t, f.callTool(t, "trace", traceArgs(testRef)), testBundleJSON)
}

func TestTraceKeepsAmbiguousRefCandidates(t *testing.T) {
	f := newToolFixture(t)
	f.trace.EXPECT().
		Trace(gomock.Any(), services.TraceRequest{Ref: testRef}).
		Return(nil, ambiguousRefError(testRef))

	assertKeepsCandidates(t, errorText(t, f.callTool(t, "trace", traceArgs(testRef))))
}

func TestTraceMapsServiceErrors(t *testing.T) {
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
			want: transport.InternalErrorMessage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			f.trace.EXPECT().Trace(gomock.Any(), gomock.Any()).Return(nil, tt.err)

			if got := errorText(t, f.callTool(t, "trace", traceArgs(testRef))); got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTraceLogsInternalCauseInsteadOfLeakingIt(t *testing.T) {
	f := newToolFixture(t)
	f.trace.EXPECT().
		Trace(gomock.Any(), gomock.Any()).
		Return(nil, internalerror.NewInternalError("walking the provenance graph failed", errors.New(testCause)))

	if got := errorText(t, f.callTool(t, "trace", traceArgs(testRef))); strings.Contains(got, testCause) {
		t.Errorf("error %q leaks the cause", got)
	}
	logged := f.logs.String()
	if !strings.Contains(logged, testCause) {
		t.Errorf("log %q does not record the cause", logged)
	}
	if !strings.Contains(logged, "trace failed") {
		t.Errorf("log %q does not name the failing tool", logged)
	}
}
