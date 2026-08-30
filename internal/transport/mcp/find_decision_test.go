package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	mock_services "lore/internal/mocks/services"
	"lore/internal/services"
)

type toolFixture struct {
	query   *mock_services.MockQueryService
	session *sdk.ClientSession
	logs    *bytes.Buffer
}

func newToolFixture(t *testing.T) toolFixture {
	t.Helper()

	ctx := context.Background()
	query := mock_services.NewMockQueryService(gomock.NewController(t))
	logs := &bytes.Buffer{}

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	serverSession, err := newServer(query, slog.New(slog.NewTextHandler(logs, nil))).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() {
		if err := clientSession.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
		if err := serverSession.Wait(); err != nil {
			t.Errorf("wait for server: %v", err)
		}
	})

	return toolFixture{query: query, session: clientSession, logs: logs}
}

func (f toolFixture) call(t *testing.T, args map[string]any) *sdk.CallToolResult {
	t.Helper()

	res, err := f.session.CallTool(context.Background(), &sdk.CallToolParams{Name: "find_decision", Arguments: args})
	if err != nil {
		t.Fatalf("call find_decision: %v", err)
	}

	return res
}

func errorText(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()

	if !res.IsError {
		t.Fatalf("result is not an error: %+v", res)
	}
	if len(res.Content) != 1 {
		t.Fatalf("error content = %d blocks, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("error content is %T, want *mcp.TextContent", res.Content[0])
	}

	return text.Text
}

func questionArgs(question string) map[string]any {
	return map[string]any{"question": question}
}

const testQuestion = "why postgres over mysql"

func TestFindDecisionReturnsBundleAsJSON(t *testing.T) {
	f := newToolFixture(t)
	f.query.EXPECT().
		FindDecision(gomock.Any(), services.FindDecisionRequest{Question: testQuestion}).
		Return(testBundle(), nil)

	res := f.call(t, questionArgs(testQuestion))

	if res.IsError {
		t.Fatalf("unexpected tool error: %s", errorText(t, res))
	}

	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	assertSameJSON(t, structured, []byte(testBundleJSON))

	if len(res.Content) != 1 {
		t.Fatalf("content = %d blocks, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("content is %T, want *mcp.TextContent", res.Content[0])
	}
	assertSameJSON(t, []byte(text.Text), []byte(testBundleJSON))
}

func TestFindDecisionReturnsEmptyBundle(t *testing.T) {
	f := newToolFixture(t)
	f.query.EXPECT().
		FindDecision(gomock.Any(), services.FindDecisionRequest{Question: testQuestion}).
		Return(&entities.EvidenceBundle{Question: testQuestion}, nil)

	res := f.call(t, questionArgs(testQuestion))

	if res.IsError {
		t.Fatalf("unexpected tool error: %s", errorText(t, res))
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	assertSameJSON(t, structured, []byte(`{
		"question": "why postgres over mysql",
		"anchor": {"kinds": []},
		"nodes": []
	}`))
}

func TestFindDecisionParsesArguments(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want services.FindDecisionRequest
	}{
		{
			name: "filters",
			args: map[string]any{
				"question": testQuestion,
				"source":   "github",
				"repo":     "github:acme/lore",
				"doc_type": "pr",
			},
			want: services.FindDecisionRequest{
				Question: testQuestion,
				Source:   "github",
				Repo:     "github:acme/lore",
				DocType:  "pr",
			},
		},
		{
			name: "event anchor",
			args: map[string]any{"question": testQuestion, "around": "incident X"},
			want: services.FindDecisionRequest{Question: testQuestion, Around: "incident X"},
		},
		{
			name: "dates cover the whole day",
			args: map[string]any{"question": testQuestion, "since": "2025-03-12", "until": "2025-04-01"},
			want: services.FindDecisionRequest{
				Question: testQuestion,
				Since:    time.Date(2025, 3, 12, 0, 0, 0, 0, time.UTC),
				Until:    time.Date(2025, 4, 1, 23, 59, 59, 0, time.UTC),
			},
		},
		{
			name: "rfc3339 timestamps",
			args: map[string]any{
				"question": testQuestion,
				"since":    "2025-03-12T09:30:00Z",
				"until":    "2025-04-01T18:00:00+02:00",
			},
			want: services.FindDecisionRequest{
				Question: testQuestion,
				Since:    testCreatedAt,
				Until:    time.Date(2025, 4, 1, 18, 0, 0, 0, time.FixedZone("", 2*60*60)),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			f.query.EXPECT().
				FindDecision(gomock.Any(), matchRequest(tt.want)).
				Return(&entities.EvidenceBundle{Question: testQuestion}, nil)

			if res := f.call(t, tt.args); res.IsError {
				t.Fatalf("unexpected tool error: %s", errorText(t, res))
			}
		})
	}
}

func matchRequest(want services.FindDecisionRequest) gomock.Matcher {
	return gomock.Cond(func(got services.FindDecisionRequest) bool {
		return got.Question == want.Question &&
			got.Around == want.Around &&
			got.Source == want.Source &&
			got.Repo == want.Repo &&
			got.DocType == want.DocType &&
			got.Since.Equal(want.Since) &&
			got.Until.Equal(want.Until)
	})
}

func TestFindDecisionRejectsMalformedWindow(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "since",
			args: map[string]any{"question": testQuestion, "since": "last tuesday"},
			want: `invalid argument: since: "last tuesday" is neither a date (YYYY-MM-DD) nor an RFC 3339 timestamp`,
		},
		{
			name: "until",
			args: map[string]any{"question": testQuestion, "until": "12/03/2025"},
			want: `invalid argument: until: "12/03/2025" is neither a date (YYYY-MM-DD) nor an RFC 3339 timestamp`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)

			if got := errorText(t, f.call(t, tt.args)); got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFindDecisionMapsServiceErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "bad request",
			err:  internalerror.NewBadRequestError("question must not be empty", nil),
			want: "invalid argument: question must not be empty",
		},
		{
			name: "not found",
			err:  internalerror.NewNotFoundError("no document matches ref PROJ-4521", nil),
			want: "not found: no document matches ref PROJ-4521",
		},
		{
			name: "precondition keeps its remediation verbatim",
			err:  internalerror.NewPreconditionError("embedder identity mismatch - run `lore sync --reembed`", nil),
			want: "embedder identity mismatch - run `lore sync --reembed`",
		},
		{
			name: "internal hides the cause",
			err:  internalerror.NewInternalError("vector search failed", errors.New("dial 10.1.2.3:5432: connection refused")),
			want: internalErrorMessage,
		},
		{
			name: "unclassified hides the cause",
			err:  errors.New("dial 10.1.2.3:5432: connection refused"),
			want: internalErrorMessage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolFixture(t)
			f.query.EXPECT().FindDecision(gomock.Any(), gomock.Any()).Return(nil, tt.err)

			if got := errorText(t, f.call(t, questionArgs(testQuestion))); got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFindDecisionLogsInternalCauseInsteadOfLeakingIt(t *testing.T) {
	const cause = "dial 10.1.2.3:5432: connection refused"

	f := newToolFixture(t)
	f.query.EXPECT().
		FindDecision(gomock.Any(), gomock.Any()).
		Return(nil, internalerror.NewInternalError("vector search failed", errors.New(cause)))

	if got := errorText(t, f.call(t, questionArgs(testQuestion))); strings.Contains(got, cause) {
		t.Errorf("error %q leaks the cause", got)
	}
	if logged := f.logs.String(); !strings.Contains(logged, cause) {
		t.Errorf("log %q does not record the cause", logged)
	}
}

func TestFindDecisionRequiresQuestion(t *testing.T) {
	f := newToolFixture(t)

	got := errorText(t, f.call(t, map[string]any{"source": "github"}))

	if !strings.Contains(got, "question") {
		t.Errorf("error = %q, want it to name the missing question", got)
	}
}

func TestFindDecisionToolDeclaration(t *testing.T) {
	f := newToolFixture(t)

	tools, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools.Tools))
	}

	tool := tools.Tools[0]
	if tool.Name != "find_decision" {
		t.Errorf("name = %q, want find_decision", tool.Name)
	}
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Errorf("annotations = %+v, want readOnlyHint", tool.Annotations)
	}
	if !strings.Contains(tool.Description, "evidence") {
		t.Errorf("description = %q, want it to explain that the result is evidence", tool.Description)
	}

	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema is %T, want map[string]any", tool.InputSchema)
	}
	if got := schema["required"]; !reflect.DeepEqual(got, []any{"question"}) {
		t.Errorf("required = %v, want [question]", got)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties is %T, want map[string]any", schema["properties"])
	}
	for _, name := range []string{"question", "around", "source", "repo", "doc_type", "since", "until"} {
		if _, declared := properties[name]; !declared {
			t.Errorf("property %q is missing from the input schema", name)
		}
	}
}
