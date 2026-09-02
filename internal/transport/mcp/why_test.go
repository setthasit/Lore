package mcp

import (
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/services"
)

const (
	testFile       = "internal/auth/auth.go"
	askOnlyRefusal = "no repositories registered — code anchoring disabled for this workspace"
)

func whyArgs(file string, start int) map[string]any {
	return map[string]any{"file": file, "line_start": start}
}

func TestWhyParsesArguments(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want services.WhyRequest
	}{
		{
			name: "file and line_start alone",
			args: whyArgs(testFile, 10),
			want: services.WhyRequest{File: testFile, LineStart: 10},
		},
		{
			name: "a whole span",
			args: map[string]any{"file": testFile, "line_start": 10, "line_end": 20},
			want: services.WhyRequest{File: testFile, LineStart: 10, LineEnd: 20},
		},
		{
			name: "repo and question",
			args: map[string]any{"file": testFile, "line_start": 10, "repo": "github:acme/lore", "question": testQuestion},
			want: services.WhyRequest{Repo: "github:acme/lore", File: testFile, LineStart: 10, Question: testQuestion},
		},
		{
			name: "the service owns the line span policy",
			args: map[string]any{"file": testFile, "line_start": 0, "line_end": -4},
			want: services.WhyRequest{File: testFile, LineEnd: -4},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			f.why.EXPECT().
				Why(gomock.Any(), tt.want).
				Return(&entities.EvidenceBundle{Question: testQuestion}, nil)

			if res := f.callTool(t, whyName, tt.args); res.IsError {
				t.Fatalf("unexpected tool error: %s", errorText(t, res))
			}
		})
	}
}

func TestWhyRequiresFileAndLineStart(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "no file", args: map[string]any{"line_start": 10}, want: "file"},
		{name: "no line_start", args: map[string]any{"file": testFile}, want: "line_start"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)

			if got := errorText(t, f.callTool(t, whyName, tt.args)); !strings.Contains(got, tt.want) {
				t.Errorf("error = %q, want it to name the missing %s", got, tt.want)
			}
		})
	}
}

func TestWhyDeliversThePreconditionRefusalVerbatim(t *testing.T) {
	f := newToolFixture(t)
	f.why.EXPECT().
		Why(gomock.Any(), gomock.Any()).
		Return(nil, internalerror.NewPreconditionError(askOnlyRefusal, nil))

	if got := errorText(t, f.callTool(t, whyName, whyArgs(testFile, 10))); got != askOnlyRefusal {
		t.Errorf("error = %q, want %q", got, askOnlyRefusal)
	}
}

func TestWhyReturnsBundleAsJSON(t *testing.T) {
	f := newToolFixture(t)
	f.why.EXPECT().
		Why(gomock.Any(), services.WhyRequest{File: testFile, LineStart: 10}).
		Return(testBundle(), nil)

	assertResultJSON(t, f.callTool(t, whyName, whyArgs(testFile, 10)), testBundleJSON)
}

func TestWhyMapsServiceErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "bad request",
			err:  internalerror.NewBadRequestError("line_start 0 must be at least 1", nil),
			want: "invalid argument: line_start 0 must be at least 1",
		},
		{
			name: "not found",
			err:  internalerror.NewNotFoundError(`repo "github:acme/other" is not registered`, nil),
			want: `not found: repo "github:acme/other" is not registered`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			f.why.EXPECT().Why(gomock.Any(), gomock.Any()).Return(nil, tt.err)

			if got := errorText(t, f.callTool(t, whyName, whyArgs(testFile, 10))); got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}
}
