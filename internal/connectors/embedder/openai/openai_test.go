package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// An obviously fake credential: these tests must never need a real one, and
	// several of them assert this exact string never reaches an error message.
	fakeKey   = "sk-fake-test-key"
	testModel = "text-embedding-3-small"
	testDims  = 4
)

// testServer answers embeddings calls and records what arrived, checking the
// parts of the request every test expects to be identical.
type testServer struct {
	*httptest.Server

	mu      sync.Mutex
	inputs  [][]string
	headers []http.Header
}

// newTestServer starts a server whose handler is called with the 1-based attempt
// number, so tests can script a different answer per attempt.
func newTestServer(t *testing.T, handler func(w http.ResponseWriter, attempt int, req request)) *testServer {
	t.Helper()

	ts := &testServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want %q", r.Method, http.MethodPost)
		}
		if r.URL.Path != embeddingsPath {
			t.Errorf("path = %q, want %q", r.URL.Path, embeddingsPath)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}

		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Model != testModel {
			t.Errorf("model = %q, want %q", req.Model, testModel)
		}

		ts.mu.Lock()
		ts.inputs = append(ts.inputs, req.Input)
		ts.headers = append(ts.headers, r.Header.Clone())
		attempt := len(ts.inputs)
		ts.mu.Unlock()

		handler(w, attempt, req)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *testServer) requests() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.inputs)
}

func (ts *testServer) batchSizes() []int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	sizes := make([]int, 0, len(ts.inputs))
	for _, inputs := range ts.inputs {
		sizes = append(sizes, len(inputs))
	}
	return sizes
}

func (ts *testServer) authorization() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.headers) == 0 {
		return ""
	}
	return ts.headers[0].Get("Authorization")
}

// waitRecorder stands in for the backoff sleep so tests observe the computed
// delays without spending them.
type waitRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (r *waitRecorder) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waits = append(r.waits, d)
	return ctx.Err()
}

func (r *waitRecorder) recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.waits)
}

func newTestEmbedder(t *testing.T, baseURL string, dims int) (*Embedder, *waitRecorder) {
	t.Helper()

	e, err := New(fakeKey, testModel, baseURL, dims)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := &waitRecorder{}
	e.sleep = rec.sleep
	return e, rec
}

// vectorFor is the server's deterministic answer for a text, distinct per text
// so misordered results are detectable.
func vectorFor(text string, dims int) []float32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	seed := float32(h.Sum32() % 4096)

	vector := make([]float32, dims)
	for i := range vector {
		vector[i] = seed + float32(i)
	}
	return vector
}

// writeVectors answers inputs in the given order of input positions.
func writeVectors(w http.ResponseWriter, inputs []string, dims int, order []int) {
	data := make([]embeddingData, 0, len(order))
	for _, i := range order {
		data = append(data, embeddingData{Index: i, Embedding: vectorFor(inputs[i], dims)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response{Data: data})
}

func reversedOrder(n int) []int {
	order := make([]int, 0, n)
	for i := n - 1; i >= 0; i-- {
		order = append(order, i)
	}
	return order
}

func assertVectors(t *testing.T, got [][]float32, texts []string, dims int) {
	t.Helper()

	if len(got) != len(texts) {
		t.Fatalf("vectors = %d, want %d", len(got), len(texts))
	}
	for i, text := range texts {
		if want := vectorFor(text, dims); !slices.Equal(got[i], want) {
			t.Errorf("vectors[%d] (text %q) = %v, want %v", i, text, got[i], want)
		}
	}
}

func TestEmbedPreservesInputOrder(t *testing.T) {
	texts := []string{"alpha", "beta", "gamma", "delta"}
	// The API does not promise response order, so answer in a shuffled one: the
	// index field is what must decide where each vector lands.
	shuffled := []int{2, 0, 3, 1}
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, req request) {
		if !slices.Equal(req.Input, texts) {
			t.Errorf("input = %v, want %v", req.Input, texts)
		}
		writeVectors(w, req.Input, testDims, shuffled)
	})

	e, rec := newTestEmbedder(t, ts.URL, testDims)
	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	assertVectors(t, got, texts, testDims)
	if n := ts.requests(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	if waits := rec.recorded(); len(waits) != 0 {
		t.Errorf("waits = %v, want none", waits)
	}
	if want := "Bearer " + fakeKey; ts.authorization() != want {
		t.Errorf("Authorization = %q, want %q", ts.authorization(), want)
	}
}

func TestEmbedSplitsBatchesOverRequestLimit(t *testing.T) {
	texts := make([]string, maxInputsPerRequest*2+7)
	for i := range texts {
		texts[i] = fmt.Sprintf("chunk-%d", i)
	}
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, req request) {
		if len(req.Input) > maxInputsPerRequest {
			t.Errorf("request inputs = %d, want <= %d", len(req.Input), maxInputsPerRequest)
		}
		writeVectors(w, req.Input, 2, reversedOrder(len(req.Input)))
	})

	e, _ := newTestEmbedder(t, ts.URL, 2)
	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	assertVectors(t, got, texts, 2)
	want := []int{maxInputsPerRequest, maxInputsPerRequest, 7}
	if sizes := ts.batchSizes(); !slices.Equal(sizes, want) {
		t.Errorf("batch sizes = %v, want %v", sizes, want)
	}
}

func TestEmbedRetriesRateLimitHonoringRetryAfter(t *testing.T) {
	texts := []string{"alpha", "beta"}

	t.Run("delta seconds", func(t *testing.T) {
		ts := newTestServer(t, func(w http.ResponseWriter, attempt int, req request) {
			if attempt == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached","code":"rate_limit_exceeded"}}`))
				return
			}
			writeVectors(w, req.Input, testDims, []int{0, 1})
		})

		e, rec := newTestEmbedder(t, ts.URL, testDims)
		got, err := e.Embed(context.Background(), texts)
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		assertVectors(t, got, texts, testDims)

		if n := ts.requests(); n != 2 {
			t.Errorf("requests = %d, want 2", n)
		}
		if waits := rec.recorded(); !slices.Equal(waits, []time.Duration{2 * time.Second}) {
			t.Errorf("waits = %v, want [2s]", waits)
		}
	})

	t.Run("http date", func(t *testing.T) {
		ts := newTestServer(t, func(w http.ResponseWriter, attempt int, req request) {
			if attempt == 1 {
				w.Header().Set("Retry-After", time.Now().Add(3*time.Second).UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			writeVectors(w, req.Input, testDims, []int{0, 1})
		})

		e, rec := newTestEmbedder(t, ts.URL, testDims)
		if _, err := e.Embed(context.Background(), texts); err != nil {
			t.Fatalf("Embed: %v", err)
		}

		waits := rec.recorded()
		if len(waits) != 1 {
			t.Fatalf("waits = %v, want one entry", waits)
		}
		// The date is resolved against wall clock, so only the bound is stable;
		// what matters is that it came from the header, not the exponential
		// schedule, whose first window never reaches two seconds.
		if waits[0] <= time.Second || waits[0] > 3*time.Second {
			t.Errorf("wait = %v, want within (1s, 3s]", waits[0])
		}
	})
}

func TestEmbedRetriesServerError(t *testing.T) {
	texts := []string{"alpha", "beta"}
	ts := newTestServer(t, func(w http.ResponseWriter, attempt int, req request) {
		if attempt == 1 {
			http.Error(w, "upstream exploded", http.StatusInternalServerError)
			return
		}
		writeVectors(w, req.Input, testDims, []int{1, 0})
	})

	e, rec := newTestEmbedder(t, ts.URL, testDims)
	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	assertVectors(t, got, texts, testDims)

	if n := ts.requests(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
	waits := rec.recorded()
	if len(waits) != 1 {
		t.Fatalf("waits = %v, want one entry", waits)
	}
	// No Retry-After: the first window is baseBackoff with equal jitter.
	if waits[0] < baseBackoff/2 || waits[0] > baseBackoff {
		t.Errorf("wait = %v, want within [%v, %v]", waits[0], baseBackoff/2, baseBackoff)
	}
}

func TestEmbedFailsFastOnClientError(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"'$.input' is invalid","code":"invalid_request_error"}}`))
	})

	e, rec := newTestEmbedder(t, ts.URL, testDims)
	_, err := e.Embed(context.Background(), []string{"alpha"})
	if err == nil {
		t.Fatal("Embed succeeded, want error")
	}

	if n := ts.requests(); n != 1 {
		t.Errorf("requests = %d, want 1 (4xx is not retried)", n)
	}
	if waits := rec.recorded(); len(waits) != 0 {
		t.Errorf("waits = %v, want none", waits)
	}
	for _, want := range []string{"400", "invalid_request_error", "'$.input' is invalid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestEmbedExhaustsRetries(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	})

	e, rec := newTestEmbedder(t, ts.URL, testDims)
	_, err := e.Embed(context.Background(), []string{"alpha"})
	if err == nil {
		t.Fatal("Embed succeeded, want error")
	}

	if n := ts.requests(); n != maxAttempts {
		t.Errorf("requests = %d, want %d", n, maxAttempts)
	}
	if waits := rec.recorded(); len(waits) != maxAttempts-1 {
		t.Errorf("waits = %v, want %d entries", waits, maxAttempts-1)
	}
	for _, want := range []string{fmt.Sprintf("after %d attempts", maxAttempts), "503"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestEmbedRespectsContextCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
		// A directive far longer than this test is willing to wait: cancelling
		// the context must cut the sleep short instead of honoring it.
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		cancel()
	})

	// The real sleep, not the recorder: this is the wait being interrupted.
	e, err := New(fakeKey, testModel, ts.URL, testDims)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	if _, err := e.Embed(ctx, []string{"alpha"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Embed took %v, want an immediate return", elapsed)
	}
	if n := ts.requests(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestEmbedRejectsDimensionMismatch(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, req request) {
		writeVectors(w, req.Input, testDims-1, []int{0})
	})

	e, rec := newTestEmbedder(t, ts.URL, testDims)
	_, err := e.Embed(context.Background(), []string{"alpha"})
	if err == nil {
		t.Fatal("Embed succeeded, want error")
	}

	want := fmt.Sprintf("returned %d dimensions, want %d", testDims-1, testDims)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
	if n := ts.requests(); n != 1 {
		t.Errorf("requests = %d, want 1 (a wrong width is not transient)", n)
	}
	if waits := rec.recorded(); len(waits) != 0 {
		t.Errorf("waits = %v, want none", waits)
	}
}

func TestEmbedRejectsMalformedResponses(t *testing.T) {
	cases := []struct {
		name  string
		texts []string
		body  string
		want  string
	}{
		{
			name:  "vector count",
			texts: []string{"alpha", "beta"},
			body:  `{"data":[{"index":0,"embedding":[1,2,3,4]}]}`,
			want:  "got 1 vectors for 2 inputs",
		},
		{
			name:  "index out of range",
			texts: []string{"alpha", "beta"},
			body:  `{"data":[{"index":0,"embedding":[1,2,3,4]},{"index":7,"embedding":[1,2,3,4]}]}`,
			want:  "response index 7 outside 0..1",
		},
		{
			name:  "duplicate index",
			texts: []string{"alpha", "beta"},
			body:  `{"data":[{"index":0,"embedding":[1,2,3,4]},{"index":0,"embedding":[1,2,3,4]}]}`,
			want:  "duplicate response index 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})

			e, _ := newTestEmbedder(t, ts.URL, testDims)
			_, err := e.Embed(context.Background(), tc.texts)
			if err == nil {
				t.Fatal("Embed succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
			if n := ts.requests(); n != 1 {
				t.Errorf("requests = %d, want 1 (a protocol violation is not transient)", n)
			}
		})
	}
}

func TestErrorsOmitAPIKey(t *testing.T) {
	// The provider echoes the credential it rejected; the error built from that
	// body is what reaches logs and user output.
	echoBody := fmt.Sprintf(`{"error":{"message":"Incorrect API key provided: %s. Check your key."}}`, fakeKey)

	cases := []struct {
		name   string
		status int
	}{
		{name: "client error", status: http.StatusUnauthorized},
		{name: "retried until exhausted", status: http.StatusTooManyRequests},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(echoBody))
			})

			e, _ := newTestEmbedder(t, ts.URL, testDims)
			_, err := e.Embed(context.Background(), []string{"alpha"})
			if err == nil {
				t.Fatal("Embed succeeded, want error")
			}
			if strings.Contains(err.Error(), fakeKey) {
				t.Errorf("error %q leaks the api key", err)
			}
			if !strings.Contains(err.Error(), "[redacted]") {
				t.Errorf("error %q does not mark the redaction", err)
			}
		})
	}
}

func TestDimensionsReportsTheConfiguredWidth(t *testing.T) {
	e, err := New(fakeKey, testModel, "", 1536)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := e.Dimensions(); got != 1536 {
		t.Fatalf("Dimensions = %d, want 1536", got)
	}

	// The width is the only vector-space component the provider owns; the host
	// composes the identity, so a narrower client must report the narrower width.
	narrower, err := New(fakeKey, testModel, "", 512)
	if err != nil {
		t.Fatalf("New (narrower): %v", err)
	}
	if got := narrower.Dimensions(); got != 512 {
		t.Errorf("Dimensions = %d, want 512", got)
	}
}

func TestNewValidatesArguments(t *testing.T) {
	cases := []struct {
		name   string
		apiKey string
		model  string
		dims   int
	}{
		{name: "no api key", model: testModel, dims: testDims},
		{name: "no model", apiKey: fakeKey, dims: testDims},
		{name: "zero dims", apiKey: fakeKey, model: testModel},
		{name: "negative dims", apiKey: fakeKey, model: testModel, dims: -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.apiKey, tc.model, "", tc.dims); err == nil {
				t.Error("New succeeded, want error")
			}
		})
	}
}

func TestNewResolvesEndpoint(t *testing.T) {
	cases := []struct {
		baseURL string
		want    string
	}{
		{baseURL: "", want: DefaultBaseURL + embeddingsPath},
		{baseURL: "https://gateway.internal", want: "https://gateway.internal" + embeddingsPath},
		{baseURL: "https://gateway.internal/", want: "https://gateway.internal" + embeddingsPath},
	}

	for _, tc := range cases {
		e, err := New(fakeKey, testModel, tc.baseURL, testDims)
		if err != nil {
			t.Fatalf("New(%q): %v", tc.baseURL, err)
		}
		if e.endpoint != tc.want {
			t.Errorf("endpoint for %q = %q, want %q", tc.baseURL, e.endpoint, tc.want)
		}
	}
}

func TestEmbedWithoutUsableTexts(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
		t.Error("server called, want no request")
		w.WriteHeader(http.StatusInternalServerError)
	})
	e, _ := newTestEmbedder(t, ts.URL, testDims)
	ctx := context.Background()

	got, err := e.Embed(ctx, nil)
	if err != nil || got != nil {
		t.Errorf("Embed(nil) = %v, %v; want nil, nil", got, err)
	}

	// An empty string is rejected before the round trip that would 400 on it.
	if _, err := e.Embed(ctx, []string{"alpha", ""}); err == nil {
		t.Error("Embed with an empty text succeeded, want error")
	} else if !strings.Contains(err.Error(), "texts[1]") {
		t.Errorf("error %q does not name the offending index", err)
	}
}
