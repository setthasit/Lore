package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	mock_services "lore/internal/mocks/services"
	"lore/internal/services"
)

const serveTimeout = 10 * time.Second

type stdioResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func TestServeAnswersToolCallsOverStdio(t *testing.T) {
	query := mock_services.NewMockQueryService(gomock.NewController(t))
	query.EXPECT().
		FindDecision(gomock.Any(), services.FindDecisionRequest{Question: testQuestion}).
		Return(testBundle(), nil)

	requests := replaceStdin(t)
	responses := replaceStdout(t)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, query) }()

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

	cancel()
	select {
	case <-served:
	case <-time.After(serveTimeout):
		t.Fatal("Serve did not return after the context was cancelled")
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

func replaceStdout(t *testing.T) *bufio.Scanner {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := reader.SetReadDeadline(time.Now().Add(serveTimeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	restore := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() {
		os.Stdout = restore
		_ = writer.Close()
		_ = reader.Close()
	})

	return bufio.NewScanner(reader)
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
