package e2e

import (
	"context"
	"encoding/json"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lore/internal/di"
	"lore/internal/entities"
	"lore/internal/transport/cli"
	"lore/internal/transport/mcp"
)

const (
	findDecisionTool = "find_decision"
	syncNowTool      = "sync_now"
)

const everySource = "all configured sources"

const (
	serveShutdownTimeout = 10 * time.Second

	commandTimeout = 30 * time.Second
)

const otherHolder = "lore-e2e-other-process/4242"

// cli keeps its exit codes unexported; this is the one it maps a bad request to.
const exitBadRequest = 2

const fixtureTokenEnv = "LORE_E2E_GITHUB_TOKEN"

type servedWorkspace struct {
	*workspace
	session *sdk.ClientSession
	stop    context.CancelFunc
	served  <-chan error
	joined  sync.Once
}

func (w *workspace) mcpServices() mcp.Services {
	return mcp.Services{
		Query:   w.query,
		Why:     w.why,
		Trace:   w.trace,
		Impact:  w.impact,
		History: w.history,
		Sync:    w.round,
		Status:  w.status,
	}
}

func serveWorkspace(t *testing.T, w *workspace) *servedWorkspace {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind a loopback port: %v", err)
	}

	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- mcp.ServeHTTP(ctx, listener, w.mcpServices(), nil) }()

	s := &servedWorkspace{workspace: w, stop: stop, served: served}
	t.Cleanup(func() { s.shutDown(t) })

	client := sdk.NewClient(&sdk.Implementation{Name: "lore-e2e", Version: "v0.0.1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: "http://" + listener.Addr().String() + mcp.EndpointPath,
	}, nil)
	if err != nil {
		t.Fatalf("connect to the served workspace over streamable http: %v", err)
	}
	s.session = session

	return s
}

// Idempotent, so a test may join the shutdown itself and still be cleaned up.
func (s *servedWorkspace) shutDown(t *testing.T) {
	t.Helper()

	s.joined.Do(func() {
		if s.session != nil {
			if err := s.session.Close(); err != nil {
				t.Errorf("close the MCP client: %v", err)
			}
		}
		s.stop()

		select {
		case err := <-s.served:
			if err != nil {
				t.Errorf("ServeHTTP() = %v, want nil once its context was cancelled", err)
			}
		case <-time.After(serveShutdownTimeout):
			t.Errorf("ServeHTTP did not return within %s of its context being cancelled", serveShutdownTimeout)
		}
	})
}

func (s *servedWorkspace) call(t *testing.T, tool string, args map[string]any) *sdk.CallToolResult {
	t.Helper()

	res, err := s.session.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call %s over streamable http: %v", tool, err)
	}

	return res
}

type bundlePayload struct {
	Question string `json:"question"`
	Anchor   struct {
		Kinds []string `json:"kinds"`
		Query string   `json:"query"`
	} `json:"anchor"`
	Nodes  []nodePayload `json:"nodes"`
	Chains [][]string    `json:"chains"`
}

type nodePayload struct {
	Doc struct {
		ID     string `json:"id"`
		Source string `json:"source"`
		URL    string `json:"url"`
	} `json:"doc"`
	Excerpt string `json:"excerpt"`
	Role    string `json:"role"`
}

type syncedPayload struct {
	Synced string `json:"synced"`
}

func decodeToolResult[T any](t *testing.T, tool string, res *sdk.CallToolResult) T {
	t.Helper()

	if res.IsError {
		t.Fatalf("%s failed: %s", tool, toolErrorText(t, res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal the %s result: %v", tool, err)
	}

	var decoded T
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode the %s result %s: %v", tool, raw, err)
	}

	return decoded
}

func toolErrorText(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()

	if !res.IsError {
		t.Fatalf("result is not an error: %+v", res)
	}

	var text strings.Builder
	for _, block := range res.Content {
		content, ok := block.(*sdk.TextContent)
		if !ok {
			t.Fatalf("error content is %T, want *mcp.TextContent", block)
		}
		text.WriteString(content.Text)
	}
	if text.Len() == 0 {
		t.Fatalf("error result carries no text: %+v", res)
	}

	return text.String()
}

func fixtureCalls(api *fixtureAPI) map[string]int {
	calls := make(map[string]int, len(githubFixtureOps))
	for _, op := range githubFixtureOps {
		calls[op] = api.callCount(op)
	}

	return calls
}

func TestServedWorkspaceAnswersFindDecisionOverStreamableHTTP(t *testing.T) {
	ctx := context.Background()
	w := newWorkspace(t, "")
	w.sync(ctx, t, "first")

	s := serveWorkspace(t, w)

	bundle := decodeToolResult[bundlePayload](t, findDecisionTool,
		s.call(t, findDecisionTool, map[string]any{"question": question}))

	if bundle.Question != question {
		t.Errorf("bundle question = %q, want %q", bundle.Question, question)
	}
	if !slices.Equal(bundle.Anchor.Kinds, []string{"query"}) {
		t.Errorf("anchor kinds = %v, want [query]", bundle.Anchor.Kinds)
	}
	if bundle.Anchor.Query != question {
		t.Errorf("anchor query = %q, want %q", bundle.Anchor.Query, question)
	}

	if len(bundle.Nodes) == 0 {
		t.Fatalf("the served bundle cites nothing for %q", question)
	}
	for _, node := range bundle.Nodes {
		if node.Doc.Source != githubSource {
			t.Errorf("node %s comes from %q, want %q", node.Doc.ID, node.Doc.Source, githubSource)
		}
		if !strings.HasPrefix(node.Doc.URL, w.api.server.URL) {
			t.Errorf("node %s cites %q, want a URL on the fixture host %s", node.Doc.ID, node.Doc.URL, w.api.server.URL)
		}
		if strings.TrimSpace(node.Excerpt) == "" {
			t.Errorf("node %s carries no excerpt", node.Doc.ID)
		}
		if node.Role != entities.RoleSeed {
			t.Errorf("node %s has role %q, want %q", node.Doc.ID, node.Role, entities.RoleSeed)
		}
	}

	top := bundle.Nodes[0]
	if top.Doc.ID != string(prDocID) {
		t.Errorf("best evidence is %s, want the pull request that argues for SQLite (%s)", top.Doc.ID, prDocID)
	}
	if !strings.Contains(strings.ToLower(top.Excerpt), "picked sqlite") {
		t.Errorf("best excerpt does not quote the decision: %q", top.Excerpt)
	}

	if len(bundle.Chains) == 0 {
		t.Errorf("the served bundle carries no chain, though the fixture documents reference each other")
	}
	chained := slices.ContainsFunc(bundle.Chains, func(chain []string) bool {
		return slices.Contains(chain, string(prDocID))
	})
	if !chained {
		t.Errorf("chains %v leave out the pull request that argues for SQLite (%s)", bundle.Chains, prDocID)
	}

	want := w.ask(ctx, t)
	if len(bundle.Nodes) != len(want.Nodes) {
		t.Fatalf("the served bundle cites %d documents, want the %d the service answers with",
			len(bundle.Nodes), len(want.Nodes))
	}
	for i, node := range bundle.Nodes {
		if node.Doc.ID != string(want.Nodes[i].Doc.ID) || node.Excerpt != want.Nodes[i].Excerpt {
			t.Errorf("served node %d is %s / %q, want %s / %q",
				i, node.Doc.ID, node.Excerpt, want.Nodes[i].Doc.ID, want.Nodes[i].Excerpt)
		}
	}

	s.shutDown(t)
}

func TestServedSyncNowRefusesWhileAnotherProcessHoldsTheLease(t *testing.T) {
	ctx := context.Background()
	w := newWorkspace(t, "")
	w.sync(ctx, t, "first")
	s := serveWorkspace(t, w)

	indexed, callsBefore := w.stats(ctx, t), fixtureCalls(w.api)

	taken, err := w.store.TryAcquireLease(ctx, otherHolder)
	if err != nil {
		t.Fatalf("take the workspace lease as %s: %v", otherHolder, err)
	}
	if !taken {
		t.Fatalf("the workspace lease was already held before %s took it", otherHolder)
	}

	refusal := toolErrorText(t, s.call(t, syncNowTool, map[string]any{}))
	for _, want := range []string{otherHolder, "already writing this index", "lease TTL"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("sync_now refusal = %q, want it to contain %q", refusal, want)
		}
	}

	refused := w.stats(ctx, t)
	if refused.Documents != indexed.Documents || refused.Chunks != indexed.Chunks || refused.Edges != indexed.Edges {
		t.Errorf("the refused round left %d documents, %d chunks and %d edges, want the %d, %d and %d it started with",
			refused.Documents, refused.Chunks, refused.Edges, indexed.Documents, indexed.Chunks, indexed.Edges)
	}
	if !slices.Equal(refused.Cursors, indexed.Cursors) {
		t.Errorf("the refused round checkpointed %+v, want %+v untouched", refused.Cursors, indexed.Cursors)
	}
	for op, calls := range fixtureCalls(w.api) {
		if calls != callsBefore[op] {
			t.Errorf("the refused round asked the fixture API for %s %d times, want the %d already made",
				op, calls, callsBefore[op])
		}
	}
	if refused.Lease == nil || refused.Lease.Holder != otherHolder {
		t.Errorf("lease after the refusal = %+v, want it still held by %s", refused.Lease, otherHolder)
	}

	if err := w.store.ReleaseLease(ctx, otherHolder); err != nil {
		t.Fatalf("release the workspace lease held by %s: %v", otherHolder, err)
	}

	ack := decodeToolResult[syncedPayload](t, syncNowTool, s.call(t, syncNowTool, map[string]any{}))
	if ack.Synced != everySource {
		t.Errorf("sync_now over the free lease synced %q, want %q", ack.Synced, everySource)
	}
	if calls := fixtureCalls(w.api); maps.Equal(calls, callsBefore) {
		t.Errorf("the round over the free lease asked the fixture API for nothing: still %+v", calls)
	}
	if synced := w.stats(ctx, t); synced.Documents != fixtureDocuments {
		t.Errorf("the round over the free lease left %d documents, want %d", synced.Documents, fixtureDocuments)
	}
}

func TestServeCommandRefusesANonLoopbackAddressWithoutTLS(t *testing.T) {
	addr := freeWildcardAddr(t)
	configPath, indexPath := writeServeConfig(t, addr)

	code, stderr := runLore(t, "--config", configPath, "serve")

	if code != exitBadRequest {
		t.Fatalf("`lore serve` exit = %d, want %d (stderr %q)", code, exitBadRequest, stderr)
	}
	for _, want := range []string{addr, "not a loopback address", "server.mtls.cert", "server.mtls.key"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "serving MCP") {
		t.Errorf("stderr = %q, want no listener announced", stderr)
	}

	if _, err := os.Stat(indexPath); err != nil {
		t.Errorf("the refusal came before the workspace was built: no index at %s (%v)", indexPath, err)
	}

	probe, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s is bound after the refusal: %v", addr, err)
	}
	if err := probe.Close(); err != nil {
		t.Errorf("close the probe listener: %v", err)
	}
}

func freeWildcardAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("reserve a free wildcard address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved address %s: %v", addr, err)
	}

	return addr
}

// The real embedder is 1536 wide, so serve gets an index of its own: the fixture one at
// fakeDims fails the width check. The scheduler tick is far out; a round would hit GitHub.
func writeServeConfig(t *testing.T, addr string) (configPath, indexPath string) {
	t.Helper()

	dir := t.TempDir()
	configPath, indexPath = filepath.Join(dir, "lore.yaml"), filepath.Join(dir, "serve.db")

	t.Setenv(fixtureTokenEnv, fixtureToken)
	t.Setenv(di.EmbedderKeyEnv, "sk-lore-e2e-not-a-real-key")

	body := "workspace: lore-e2e\n" +
		"index_path: " + strconv.Quote(indexPath) + "\n" +
		"sources:\n" +
		"  github:\n" +
		"    token_env: " + fixtureTokenEnv + "\n" +
		"    repos: [" + fixtureRepo + "]\n" +
		"scheduler:\n" +
		"  interval: 24h\n" +
		"server:\n" +
		"  http_addr: " + strconv.Quote(addr) + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", configPath, err)
	}

	return configPath, indexPath
}

// The command runs on its own goroutine so that a `serve` which binds instead of refusing
// fails this test rather than blocking it until the whole package times out.
func runLore(t *testing.T, args ...string) (exitCode int, stderr string) {
	t.Helper()

	captured, err := os.CreateTemp(t.TempDir(), "stderr-*.log")
	if err != nil {
		t.Fatalf("capture stderr: %v", err)
	}

	realArgs, realStderr := os.Args, os.Stderr
	os.Args, os.Stderr = append([]string{"lore"}, args...), captured

	returned := make(chan int, 1)
	go func() { returned <- cli.Main() }()

	select {
	case exitCode = <-returned:
		os.Args, os.Stderr = realArgs, realStderr
	case <-time.After(commandTimeout):
		// Left swapped on purpose: the command is still reading them.
		t.Fatalf("`lore %s` has not returned after %s", strings.Join(args, " "), commandTimeout)
	}

	if err := captured.Close(); err != nil {
		t.Fatalf("close the captured stderr: %v", err)
	}
	written, err := os.ReadFile(captured.Name())
	if err != nil {
		t.Fatalf("read the captured stderr: %v", err)
	}

	return exitCode, string(written)
}
