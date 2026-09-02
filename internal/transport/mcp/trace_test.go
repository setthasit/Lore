package mcp

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/services"
	"lore/internal/transport"
)

const (
	testRef   = "abc1234"
	testCause = "dial 10.1.2.3:5432: connection refused"
)

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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			f.trace.EXPECT().
				Trace(gomock.Any(), tt.want).
				Return(&entities.EvidenceBundle{Question: "provenance of " + testRef}, nil)

			if res := f.callTool(t, "trace", tt.args); res.IsError {
				t.Fatalf("unexpected tool error: %s", errorText(t, res))
			}
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
