package mcp

import (
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type httpFixture struct {
	endpoint string
	served   <-chan error
	stop     context.CancelFunc
}

func serveOverHTTP(t *testing.T) httpFixture {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- ServeHTTP(ctx, listener, newMockedTools(t).services(), nil) }()
	t.Cleanup(stop)

	return httpFixture{endpoint: "http://" + listener.Addr().String(), served: served, stop: stop}
}

func (f httpFixture) connect(t *testing.T) *sdk.ClientSession {
	t.Helper()

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0.0.1"}, nil)
	session, err := client.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: f.endpoint + EndpointPath}, nil)
	if err != nil {
		t.Fatalf("connect over streamable http: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	})
	return session
}

func (f httpFixture) listTools(t *testing.T) []string {
	t.Helper()

	advertised, err := f.connect(t).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	names := make([]string, 0, len(advertised.Tools))
	for _, tool := range advertised.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestServeHTTPAdvertisesTheSameToolsAsStdio(t *testing.T) {
	names := serveOverHTTP(t).listTools(t)

	want := append([]string{"find_decision", whyName, "trace", "impact_of", historyName}, syncToolNames...)
	for _, tool := range want {
		if !slices.Contains(names, tool) {
			t.Errorf("tools = %v, want it to contain %q", names, tool)
		}
	}
	if len(names) != len(want) {
		t.Errorf("tools = %v, want exactly %d", names, len(want))
	}
}

func TestServeHTTPServesNoPathButTheEndpoint(t *testing.T) {
	f := serveOverHTTP(t)

	for _, path := range []string{"/", "/mcp/", "/mcp/extra", "/metrics"} {
		res, err := http.Post(f.endpoint+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s = %d (%s), want %d", path, res.StatusCode, body, http.StatusNotFound)
		}
	}
}

func TestServeHTTPReturnsCleanlyAndStopsListeningWhenCancelled(t *testing.T) {
	f := serveOverHTTP(t)
	f.listTools(t)

	f.stop()

	select {
	case err := <-f.served:
		if err != nil {
			t.Fatalf("ServeHTTP() = %v, want nil after its context was cancelled", err)
		}
	case <-time.After(serveTimeout):
		t.Fatal("ServeHTTP did not return after its context was cancelled")
	}

	if res, err := http.Get(f.endpoint + EndpointPath); err == nil {
		_ = res.Body.Close()
		t.Fatalf("GET %s succeeded after shutdown, want the listener closed", f.endpoint+EndpointPath)
	}
}
