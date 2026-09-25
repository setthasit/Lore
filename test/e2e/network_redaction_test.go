package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"testing"
	"time"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	lorev1 "github.com/setthasit/Lore/api/proto/lore/v1"
	"github.com/setthasit/Lore/app"
	"github.com/setthasit/Lore/internal/secrets"
	"github.com/setthasit/Lore/sdk"
)

const (
	networkTokenEnv = "LORE_E2E_NETWORK_TOKEN"
	networkToken    = "fake-net-token-7Kq2"

	citingPluginName = "e2e-citing"
	citedURL         = "https://docs.example.test/cited"
	twinURL          = "https://docs.example.test/twin"
	citedIDStem      = citingPluginName + ":page:cited-token="
	twinIDStem       = citingPluginName + ":page:twin-a-token="

	traceTool       = "trace"
	loopbackAnyPort = "127.0.0.1:0"
	servingMCP      = "lore: serving MCP on "
	servingGRPC     = "lore: serving gRPC on "
)

type citingSourcePlugin struct{}

var _ lore.SourcePlugin = citingSourcePlugin{}

func (citingSourcePlugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       citingPluginName,
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "compiled-in source whose document ids carry its token",
		Secrets:    []lore.Secret{{Key: "token"}},
	}
}

func (citingSourcePlugin) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	return citingConnector{name: c.Instance, token: c.Secret("token")}, nil
}

type citingConnector struct{ name, token string }

func (c citingConnector) Name() string { return c.name }

func (c citingConnector) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) {
		yield(lore.Batch{
			Docs: []lore.Document{
				c.page("cited-token=", citedURL),
				c.page("twin-a-token=", twinURL),
				c.page("twin-b-token=", twinURL),
			},
			Cursor: lore.Cursor{"page": "1"},
		}, nil)
	}
}

func (c citingConnector) page(stem, url string) lore.Document {
	at := time.Date(2026, time.March, 12, 9, 30, 0, 0, time.UTC)
	return lore.Document{
		ID:        lore.DocID(c.name + ":page:" + stem + c.token),
		Source:    c.name,
		Type:      lore.DocTypePage,
		Title:     "Storage design",
		Body:      "we index on sqlite",
		URL:       url,
		CreatedAt: at,
		UpdatedAt: at,
	}
}

func sourceReadingNetworkToken(plugin string) string {
	return "  - use: " + plugin + "\n" +
		"    with:\n" +
		"      token: ${env:" + networkTokenEnv + "}\n"
}

type servedLore struct {
	mcpEndpoint string
	queries     lorev1.QueryServiceClient
	syncs       lorev1.SyncServiceClient
}

// Interrupts the whole test process on cleanup; callers must not run in parallel.
func serveLore(t *testing.T, sourcePlugin lore.Plugin) servedLore {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("windows cannot interrupt its own process")
	}
	t.Setenv(networkTokenEnv, networkToken)
	config := leakyWorkspace(t, "sources:\n"+sourceReadingNetworkToken(sourcePlugin.Manifest().Name)+
		"scheduler:\n"+
		"  interval: 24h\n")

	logs, logged, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	stdout := captureFile(t, "stdout")
	realArgs, realStdout, realStderr := os.Args, os.Stdout, os.Stderr
	os.Args = []string{"lore", "serve", "--config", config, "--http", loopbackAnyPort, "--grpc", loopbackAnyPort}
	os.Stdout, os.Stderr = stdout, logged

	caught := make(chan os.Signal, 1)
	signal.Notify(caught, os.Interrupt)
	t.Cleanup(func() { signal.Stop(caught) })

	returned := make(chan int, 1)
	go func() { returned <- app.Run(sourcePlugin, stubEmbedderPlugin{}) }()
	announced, drained := make(chan servedAddrs, 1), make(chan string, 1)
	go func() { drained <- drainServeLog(logs, announced) }()

	stopped := func() string {
		os.Args, os.Stdout, os.Stderr = realArgs, realStdout, realStderr
		_ = logged.Close()
		return <-drained
	}
	var addrs servedAddrs
	select {
	case addrs = <-announced:
	case code := <-returned:
		t.Fatalf("`lore serve` exited %d before serving; stderr = %q", code, stopped())
	case <-time.After(commandTimeout):
		t.Fatalf("`lore serve` did not announce its listeners within %s", commandTimeout)
	}

	t.Cleanup(func() {
		interruptSelf(t)
		select {
		case code := <-returned:
			if code != exitOK {
				t.Errorf("`lore serve` exit = %d after an interrupt, want %d", code, exitOK)
			}
			stopped()
		case <-time.After(commandTimeout):
			t.Errorf("`lore serve` has not returned %s after an interrupt", commandTimeout)
		}
	})

	conn, err := grpclib.NewClient(addrs.grpc, grpclib.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addrs.grpc, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return servedLore{
		mcpEndpoint: addrs.mcp,
		queries:     lorev1.NewQueryServiceClient(conn),
		syncs:       lorev1.NewSyncServiceClient(conn),
	}
}

type servedAddrs struct{ mcp, grpc string }

func drainServeLog(logs *os.File, announced chan<- servedAddrs) string {
	defer func() { _ = logs.Close() }()

	var (
		all   strings.Builder
		addrs servedAddrs
	)
	for lines := bufio.NewScanner(logs); lines.Scan(); {
		line := lines.Text()
		all.WriteString(line + "\n")
		if endpoint, ok := strings.CutPrefix(line, servingMCP); ok {
			addrs.mcp = endpoint
		}
		if addr, ok := strings.CutPrefix(line, servingGRPC); ok && addrs.mcp != "" {
			addrs.grpc = addr
			announced <- addrs
			addrs.mcp = ""
		}
	}
	return all.String()
}

func interruptSelf(t *testing.T) {
	t.Helper()

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find this process: %v", err)
	}
	if err := self.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt `lore serve`: %v", err)
	}
}

func (s servedLore) syncOverGRPC(t *testing.T) {
	t.Helper()

	synced, err := s.syncs.Trigger(t.Context(), &lorev1.TriggerRequest{})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if failures := synced.GetFailures(); len(failures) > 0 {
		t.Fatalf("Trigger failures = %v, want a clean round", failures)
	}
}

func (s servedLore) postTool(t *testing.T, tool, arguments string) string {
	t.Helper()

	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + arguments + `}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.mcpEndpoint, strings.NewReader(call))
	if err != nil {
		t.Fatalf("build the %s request: %v", tool, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", tool, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read the %s response: %v", tool, err)
	}
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%s response = %d %s %q, want 200 text/event-stream", tool, res.StatusCode, res.Header.Get("Content-Type"), body)
	}

	answered := false
	for line := range strings.Lines(string(body)) {
		data, isData := strings.CutPrefix(strings.TrimRight(line, "\n"), "data: ")
		if !isData {
			continue
		}
		var message struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(data), &message); err != nil || message.JSONRPC != "2.0" {
			t.Fatalf("%s event %q is not JSON-RPC 2.0: %v", tool, data, err)
		}
		answered = answered || (message.ID == 1 && message.Result != nil)
	}
	if !answered {
		t.Fatalf("%s response %q carries no result for the call", tool, body)
	}
	return string(body)
}

func assertRedacted(t *testing.T, surface, emitted, want string) {
	t.Helper()

	if !strings.Contains(emitted, want) {
		t.Errorf("%s = %q, want it to contain %q", surface, emitted, want)
	}
	if strings.Contains(emitted, networkToken) {
		t.Errorf("%s = %q, want it free of the token", surface, emitted)
	}
}

func TestAnMCPOverHTTPSyncFailureNamingASecretReachesTheClientRedacted(t *testing.T) {
	served := serveLore(t, misfilingSourcePlugin{})

	body := served.postTool(t, syncNowTool, `{}`)

	assertRedacted(t, "sync_now response body", body, misfiledIDPrefix+secrets.Placeholder)
}

func TestAnIndexedDocumentIDHoldingASecretReachesNetworkClientsRedacted(t *testing.T) {
	served := serveLore(t, citingSourcePlugin{})
	served.syncOverGRPC(t)

	body := served.postTool(t, traceTool, `{"ref":"`+citedURL+`"}`)
	assertRedacted(t, "trace response body over MCP HTTP", body, citedIDStem+secrets.Placeholder)

	traced, err := served.queries.Trace(t.Context(), &lorev1.TraceRequest{Ref: citedURL, Synthesize: proto.Bool(false)})
	if err != nil {
		t.Fatalf("Trace(%s): %v", citedURL, err)
	}
	if got, want := traced.GetBundle().GetAnchor().GetDoc().GetId(), citedIDStem+secrets.Placeholder; got != want {
		t.Errorf("Trace anchor id = %q, want %q", got, want)
	}
	assertRedacted(t, "Trace response over gRPC", prototext.Format(traced), citedIDStem+secrets.Placeholder)
}

func TestAGRPCErrorQuotingASecretReachesTheClientRedacted(t *testing.T) {
	served := serveLore(t, citingSourcePlugin{})
	served.syncOverGRPC(t)

	_, err := served.queries.Trace(t.Context(), &lorev1.TraceRequest{Ref: twinURL, Synthesize: proto.Bool(false)})

	refused, _ := status.FromError(err)
	if refused.Code() != codes.InvalidArgument {
		t.Fatalf("Trace(%s) = %v, want %s naming both documents at that URL", twinURL, err, codes.InvalidArgument)
	}
	assertRedacted(t, "Trace status message", refused.Message(), twinIDStem+secrets.Placeholder)
}
