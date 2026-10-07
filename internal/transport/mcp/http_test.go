package mcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/secrets"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport"
)

type httpFixture struct {
	endpoint     string
	served       <-chan error
	stop         context.CancelFunc
	cancelServer context.CancelFunc
	client       *http.Client
	session      *sdk.ClientSession
}

func serveOverHTTP(t *testing.T) *httpFixture {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	return startHTTPFixture(t, listener, newMockedTools(t).services())
}

func startHTTPFixture(t *testing.T, listener net.Listener, svc transport.Services) *httpFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	f := &httpFixture{
		endpoint:     "http://" + listener.Addr().String(),
		served:       served,
		cancelServer: cancel,
		client:       &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
	}
	f.stop = func() {
		f.closeClient(t)
		cancel()
	}
	t.Cleanup(func() {
		f.stop()
		f.assertStopped(t)
	})
	go func() {
		served <- ServeHTTP(ctx, listener, svc, nil, &secrets.Sink{}, slog.New(slog.DiscardHandler))
		close(served)
	}()

	return f
}

func (f *httpFixture) connect(t *testing.T) *sdk.ClientSession {
	t.Helper()

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0.0.1"}, nil)
	session, err := client.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: f.endpoint + EndpointPath, HTTPClient: f.client}, nil)
	if err != nil {
		t.Fatalf("connect over streamable http: %v", err)
	}
	f.session = session
	return session
}

func (f *httpFixture) closeClient(t *testing.T) {
	t.Helper()
	if f.session != nil {
		if err := f.session.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}
	f.client.CloseIdleConnections()
}

func (f *httpFixture) listTools(t *testing.T) []string {
	t.Helper()

	session := f.connect(t)
	defer f.closeClient(t)
	advertised, err := session.ListTools(context.Background(), nil)
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
		res, err := f.client.Post(f.endpoint+path, "application/json", strings.NewReader("{}"))
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
	f.assertStopped(t)
}

func (f *httpFixture) assertStopped(t *testing.T) {
	t.Helper()

	select {
	case err := <-f.served:
		if err != nil {
			t.Fatalf("ServeHTTP() = %v, want nil after its context was cancelled", err)
		}
	case <-time.After(serveTimeout):
		t.Fatal("ServeHTTP did not return after its context was cancelled")
	}

	if res, err := f.client.Get(f.endpoint + EndpointPath); err == nil {
		_ = res.Body.Close()
		t.Fatalf("GET %s succeeded after shutdown, want the listener closed", f.endpoint+EndpointPath)
	}
}

type responseEndListener struct {
	net.Listener
	dialStarted <-chan struct{}
	unused      chan *unusedHTTPConnection
	accepted    int
}

func (l *responseEndListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.accepted++
	if l.accepted == 1 {
		return &responseEndConnection{Conn: conn, dialStarted: l.dialStarted}, nil
	}
	if l.accepted == 2 {
		unused := &unusedHTTPConnection{Conn: conn, closed: make(chan struct{}, 1)}
		l.unused <- unused
		return unused, nil
	}
	return conn, nil
}

type responseEndConnection struct {
	net.Conn
	dialStarted <-chan struct{}
}

func (c *responseEndConnection) Write(p []byte) (int, error) {
	if bytes.HasSuffix(p, []byte("0\r\n\r\n")) {
		select {
		case <-c.dialStarted:
		case <-time.After(serveTimeout):
			return 0, context.DeadlineExceeded
		}
	}
	return c.Conn.Write(p)
}

type unusedHTTPConnection struct {
	net.Conn
	readBytes atomic.Int64
	closed    chan struct{}
}

func (c *unusedHTTPConnection) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.readBytes.Add(int64(n))
	return n, err
}

func (c *unusedHTTPConnection) Close() error {
	err := c.Conn.Close()
	select {
	case c.closed <- struct{}{}:
	default:
	}
	return err
}

func TestHTTPFixtureClosesSpeculativeConnectionsAfterCompletedRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dialStarted, reused := make(chan struct{}), make(chan struct{}, 1)
	observed := &responseEndListener{Listener: listener, dialStarted: dialStarted, unused: make(chan *unusedHTTPConnection, 1)}
	f := startHTTPFixture(t, observed, newMockedTools(t).services())
	clientTransport := f.client.Transport.(*http.Transport)
	dial := clientTransport.DialContext
	var dials atomic.Int32
	clientTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) == 2 {
			close(dialStarted)
			select {
			case <-reused:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return dial(ctx, network, address)
	}
	session := f.connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), serveTimeout)
	defer cancel()
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if info.Reused {
			select {
			case reused <- struct{}{}:
			default:
			}
		}
	}}
	advertised, err := session.ListTools(httptrace.WithClientTrace(ctx, trace), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(advertised.Tools) == 0 {
		t.Fatal("completed request advertised no tools")
	}
	var unused *unusedHTTPConnection
	select {
	case unused = <-observed.unused:
	case <-ctx.Done():
		t.Fatal("speculative connection was not accepted")
	}
	f.closeClient(t)
	select {
	case <-unused.closed:
	case <-time.After(time.Second):
		t.Fatal("unused connection stayed open after client cleanup")
	}
	if got := unused.readBytes.Load(); got != 0 {
		t.Fatalf("speculative connection read %d bytes, want no request", got)
	}
	f.stop()
	f.assertStopped(t)
}

type httpToolResult struct {
	result *sdk.CallToolResult
	err    error
}

func activeHTTPToolCall(t *testing.T) (*httpFixture, <-chan httpToolResult, context.CancelFunc) {
	t.Helper()
	tools := newMockedTools(t)
	started := make(chan struct{})
	releaseCtx, release := context.WithCancel(context.Background())
	tools.query.EXPECT().FindDecision(gomock.Any(), services.FindDecisionRequest{Question: testQuestion}).DoAndReturn(
		func(context.Context, services.FindDecisionRequest) (*entities.EvidenceBundle, error) {
			close(started)
			<-releaseCtx.Done()
			return testBundle(), nil
		})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		release()
		t.Fatal(err)
	}
	f := startHTTPFixture(t, listener, tools.services())
	t.Cleanup(release)
	session := f.connect(t)
	returned := make(chan httpToolResult, 1)
	go func() {
		res, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "find_decision", Arguments: questionArgs(testQuestion)})
		returned <- httpToolResult{res, err}
	}()
	select {
	case <-started:
	case <-time.After(serveTimeout):
		t.Fatal("tool did not start")
	}
	f.client.CloseIdleConnections()
	return f, returned, release
}

func TestServeHTTPDrainsActiveToolCallsWhenCancelled(t *testing.T) {
	f, returned, release := activeHTTPToolCall(t)
	f.cancelServer()
	select {
	case err := <-f.served:
		t.Fatalf("shutdown returned before tool release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case result := <-returned:
		t.Fatalf("call returned before release: %+v", result)
	default:
	}
	release()
	select {
	case result := <-returned:
		if result.err != nil {
			t.Fatalf("active call failed: %v", result.err)
		}
		assertResultJSON(t, result.result, testBundleJSON)
	case <-time.After(serveTimeout):
		t.Fatal("active call did not drain")
	}
	f.assertStopped(t)
}

func TestServeHTTPTimesOutActiveToolCallsWhenCancelled(t *testing.T) {
	f, returned, release := activeHTTPToolCall(t)
	start := time.Now()
	f.cancelServer()
	select {
	case err := <-f.served:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown = %v, want context deadline exceeded", err)
		}
		want := "the MCP HTTP server dropped tool calls that were still running at shutdown: context deadline exceeded"
		if err.Error() != want {
			t.Fatalf("shutdown = %q, want %q", err, want)
		}
	case <-time.After(serveTimeout):
		t.Fatal("shutdown did not return")
	}
	if elapsed := time.Since(start); elapsed < shutdownGrace || elapsed >= serveTimeout {
		t.Fatalf("shutdown took %s, want the original grace", elapsed)
	}
	select {
	case result := <-returned:
		if result.err == nil || result.result != nil {
			t.Fatalf("call = %+v, want failure without a result", result)
		}
	case <-time.After(serveTimeout):
		t.Fatal("forced-close call did not fail")
	}
	release()
	f.assertStopped(t)
}
