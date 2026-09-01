package mcp

import (
	"reflect"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/services"
)

func historyArgs(path string) map[string]any {
	return map[string]any{"path": path}
}

func TestHistoryOfParsesArguments(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want services.HistoryRequest
	}{
		{
			name: "path alone",
			args: historyArgs(testFile),
			want: services.HistoryRequest{File: testFile},
		},
		{
			name: "repo, limit and before",
			args: map[string]any{
				"path":   testFile,
				"repo":   "github:acme/lore",
				"limit":  5,
				"before": "9f8e7d6",
			},
			want: services.HistoryRequest{
				Repo:   "github:acme/lore",
				File:   testFile,
				Limit:  5,
				Before: "9f8e7d6",
			},
		},
		{
			name: "the service owns the limit policy",
			args: map[string]any{"path": testFile, "limit": 5000},
			want: services.HistoryRequest{File: testFile, Limit: 5000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			f.history.EXPECT().
				HistoryOf(gomock.Any(), tt.want).
				Return(&entities.EvidenceBundle{Question: testQuestion}, nil)

			if res := f.callTool(t, historyName, tt.args); res.IsError {
				t.Fatalf("unexpected tool error: %s", errorText(t, res))
			}
		})
	}
}

func TestHistoryOfRequiresPath(t *testing.T) {
	f := newToolFixture(t)

	got := errorText(t, f.callTool(t, historyName, map[string]any{"repo": "github:acme/lore"}))

	if !strings.Contains(got, "path") {
		t.Errorf("error = %q, want it to name the missing path", got)
	}
}

func TestHistoryOfReturnsBundleAsJSON(t *testing.T) {
	f := newToolFixture(t)
	f.history.EXPECT().
		HistoryOf(gomock.Any(), services.HistoryRequest{File: testFile}).
		Return(testBundle(), nil)

	assertBundleResult(t, f.callTool(t, historyName, historyArgs(testFile)), testBundleJSON)
}

func TestHistoryOfDeliversThePreconditionRefusalVerbatim(t *testing.T) {
	f := newToolFixture(t)
	f.history.EXPECT().
		HistoryOf(gomock.Any(), gomock.Any()).
		Return(nil, internalerror.NewPreconditionError(askOnlyRefusal, nil))

	if got := errorText(t, f.callTool(t, historyName, historyArgs(testFile))); got != askOnlyRefusal {
		t.Errorf("error = %q, want %q", got, askOnlyRefusal)
	}
}

func TestHistoryOfMapsServiceErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "bad request",
			err:  internalerror.NewBadRequestError(`before "9f" matches both 9f8e7d6 and 9f1a2b3 — retry with a full SHA`, nil),
			want: `invalid argument: before "9f" matches both 9f8e7d6 and 9f1a2b3 — retry with a full SHA`,
		},
		{
			name: "not found",
			err:  internalerror.NewNotFoundError(`before "deadbee" names no commit in the history of `+testFile, nil),
			want: `not found: before "deadbee" names no commit in the history of ` + testFile,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			f.history.EXPECT().HistoryOf(gomock.Any(), gomock.Any()).Return(nil, tt.err)

			if got := errorText(t, f.callTool(t, historyName, historyArgs(testFile))); got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHistoryOfToolDeclaration(t *testing.T) {
	f := newToolFixture(t)

	tool := f.declaration(t, historyName)

	if !strings.Contains(tool.Description, "blamed_shas") {
		t.Errorf("description = %q, want it to spell out the paging cursor", tool.Description)
	}
	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema is %T, want map[string]any", tool.InputSchema)
	}
	if got := schema["required"]; !reflect.DeepEqual(got, []any{"path"}) {
		t.Errorf("required = %v, want [path]", got)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties is %T, want map[string]any", schema["properties"])
	}
	for _, name := range []string{"path", "repo", "limit", "before"} {
		if _, declared := properties[name]; !declared {
			t.Errorf("input schema does not declare %q", name)
		}
	}

	for field, phrases := range map[string][]string{
		"before": {"blamed_shas", "older", "exhausted"},
		"limit":  {"cap"},
	} {
		doc := fieldDescription(t, properties, field)
		for _, phrase := range phrases {
			if !strings.Contains(doc, phrase) {
				t.Errorf("description of %q = %q, want it to carry %q", field, doc, phrase)
			}
		}
	}
}

func fieldDescription(t *testing.T, properties map[string]any, field string) string {
	t.Helper()

	property, ok := properties[field].(map[string]any)
	if !ok {
		t.Fatalf("property %q is %T, want map[string]any", field, properties[field])
	}
	doc, ok := property["description"].(string)
	if !ok {
		t.Fatalf("description of %q is %T, want string", field, property["description"])
	}

	return doc
}
