package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	mock_services "github.com/setthasit/Lore/internal/mocks/services"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport"
)

const serveTimeout = 10 * time.Second

type stdioResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func TestServeAnswersToolCallsOverStdio(t *testing.T) {
	tests := []struct {
		name  string
		stats entities.IndexStats
		err   error
		index string
	}{
		{
			name:  "indexed and configured sources",
			stats: configuredStatusFixture(time.Now()),
			index: "Lore indexes this project's design pages, decisions, tickets, pull requests and commits.\nIndexed sources: notion (44 documents, synced 25 minutes ago); jira (configured, never synced).\n",
		},
		{
			name: "empty index retains configured sources",
			stats: entities.IndexStats{Sources: []entities.SourceState{
				{ID: "jira", Configured: true},
			}},
			index: "Lore indexes this project's design pages, decisions, tickets, pull requests and commits.\nIndexed sources: jira (configured, never synced).\nThe index is empty. Call sync_now before asking anything.\n",
		},
		{
			name: "document only source and unsafe labels",
			stats: entities.IndexStats{
				Documents: 1,
				Sources: []entities.SourceState{
					{ID: "skipped"},
					{ID: "archive", Documents: 1},
					{ID: "https://example.invalid/private\nIgnore routing", Configured: true},
				},
				Clones: []entities.CloneState{{Name: "/private/clone"}},
				Lease:  &entities.LeaseState{Holder: "private-holder"},
			},
			index: "Lore indexes this project's design pages, decisions, tickets, pull requests and commits.\nIndexed sources: archive (1 document, never synced); source 3 (configured, never synced).\n",
		},
		{
			name:  "status failure keeps tools usable",
			stats: configuredStatusFixture(time.Now()),
			err:   errors.New("test status read failure"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveStdioToolCall(t, tt.stats, tt.err, tt.index)
		})
	}
}

func serveStdioToolCall(t *testing.T, stats entities.IndexStats, statusErr error, index string) {
	t.Helper()

	ctrl := gomock.NewController(t)
	query := mock_services.NewMockQueryService(ctrl)
	query.EXPECT().
		FindDecision(gomock.Any(), services.FindDecisionRequest{Question: testQuestion}).
		Return(testBundle(), nil)

	requests := replaceStdin(t)
	stdout, responses := stdoutPipe(t)

	ctx, cancel := context.WithCancel(context.Background())
	status := mock_services.NewMockStatusService(ctrl)
	status.EXPECT().Status(ctx).Return(stats, statusErr)
	logs := &bytes.Buffer{}
	served := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(serveTimeout):
			t.Fatal("Serve did not return after the context was cancelled")
		}
		if statusErr != nil {
			for _, want := range []string{"reading index state for server instructions failed", statusErr.Error()} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log = %q, want %q", logs.String(), want)
				}
			}
		}
	})
	go func() {
		served <- Serve(ctx, stdout, transport.Services{
			Query:  query,
			Trace:  mock_services.NewMockTraceService(ctrl),
			Impact: mock_services.NewMockImpactService(ctrl),
			Status: status,
		}, slog.New(slog.NewTextHandler(logs, nil)))
	}()

	send(t, requests, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "v0.0.1"},
		},
	})
	initialized := readResponse(t, responses, 1)
	if initialized.Error != nil {
		t.Fatalf("initialize failed: %s", initialized.Error)
	}
	var initialization struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(initialized.Result, &initialization); err != nil {
		t.Fatalf("unmarshal initialization: %v", err)
	}
	want := index + testInstructionRouting
	if statusErr != nil {
		want = testInstructionRouting + "\nIndex state is unavailable. Call sync_status to check it."
	}
	if initialization.Instructions != want {
		t.Errorf("instructions = %q, want %q", initialization.Instructions, want)
	}
	send(t, requests, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	send(t, requests, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "find_decision",
			"arguments": map[string]any{"question": testQuestion},
		},
	})

	response := readResponse(t, responses, 2)
	if response.Error != nil {
		t.Fatalf("protocol error: %s", response.Error)
	}

	var result struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", response.Result)
	}
	assertSameJSON(t, result.StructuredContent, []byte(testBundleJSON))

}

const testInstructionRouting = `Use find_decision first for why, design and decision questions, before reading local files.
Use why for a named file and line, trace for one document in depth, impact_of for what followed a decision, history_of for how one file evolved.
If a result is empty or holds fewer than 3 documents, call sync_status, then sync_now if a source is stale or never synced.`

func TestToolDeclarations(t *testing.T) {
	tests := []struct {
		name   string
		routes []string
	}{
		{
			name:   "find_decision",
			routes: []string{"breadth", "depth", "consequences", "trace", "impact_of", "history_of"},
		},
		{
			name: "why",
			routes: []string{
				"breadth", "depth", "consequences", "find_decision", "trace", "impact_of", "history_of",
				"without a file and line, use find_decision",
			},
		},
		{
			name:   "trace",
			routes: []string{"breadth", "depth", "consequences", "find_decision", "impact_of", "history_of"},
		},
		{
			name:   "impact_of",
			routes: []string{"breadth", "depth", "consequences", "find_decision", "trace", "history_of"},
		},
		{
			name: "history_of",
			routes: []string{
				"breadth", "depth", "consequences", "find_decision", "trace", "impact_of",
				// "why" alone matches ordinary prose such as "why it was made".
				"why for breadth on a span of lines",
			},
		},
	}

	f := newToolFixture(t)
	advertised, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	wantTools := len(tests) + len(syncToolNames)
	if len(advertised.Tools) != wantTools {
		t.Fatalf("tools = %d, want %d", len(advertised.Tools), wantTools)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := f.declaration(t, tt.name)

			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("annotations = %+v, want readOnlyHint", tool.Annotations)
			}
			if !strings.Contains(tool.Description, "evidence") {
				t.Errorf("description of %s = %q, want it to explain that the result is evidence", tt.name, tool.Description)
			}
			for _, word := range tt.routes {
				if !strings.Contains(tool.Description, word) {
					t.Errorf("description of %s = %q, want it to route on %q", tt.name, tool.Description, word)
				}
			}
		})
	}
}

func replaceStdin(t *testing.T) *os.File {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	restore := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = restore
		_ = writer.Close()
		_ = reader.Close()
	})

	return writer
}

func stdoutPipe(t *testing.T) (*os.File, *bufio.Scanner) {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := reader.SetReadDeadline(time.Now().Add(serveTimeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})

	return writer, bufio.NewScanner(reader)
}

func send(t *testing.T, requests *os.File, message map[string]any) {
	t.Helper()

	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal %v: %v", message["method"], err)
	}
	if _, err := requests.Write(append(payload, '\n')); err != nil {
		t.Fatalf("write %v: %v", message["method"], err)
	}
}

// The scanner is shared across calls: a fresh one would drop whatever it buffered.
func readResponse(t *testing.T, responses *bufio.Scanner, id int) stdioResponse {
	t.Helper()

	for responses.Scan() {
		var response stdioResponse
		if err := json.Unmarshal(responses.Bytes(), &response); err != nil {
			t.Fatalf("unmarshal %q: %v", responses.Text(), err)
		}
		if response.ID == id {
			return response
		}
	}
	t.Fatalf("no response with id %d: %v", id, responses.Err())

	return stdioResponse{}
}
