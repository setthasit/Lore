package ollama

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

	"lore/internal/connectors/httpretry"
)

const (
	testModel = "nomic-embed-text"
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
		if r.URL.Path != embedPath {
			t.Errorf("path = %q, want %q", r.URL.Path, embedPath)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want no credential on a local daemon", got)
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

	e, err := New(testModel, baseURL, dims)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := &waitRecorder{}
	e.sleep = rec.sleep
	return e, rec
}

// vectorFor is the daemon's deterministic answer for a text, distinct per text
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

func writeVectors(w http.ResponseWriter, inputs []string, dims int) {
	embeddings := make([][]float32, 0, len(inputs))
	for _, input := range inputs {
		embeddings = append(embeddings, vectorFor(input, dims))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response{Embeddings: embeddings})
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

func TestEmbedBatchesInputsAndPreservesOrder(t *testing.T) {
	texts := []string{"alpha", "beta", "gamma", "delta"}
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, req request) {
		if !slices.Equal(req.Input, texts) {
			t.Errorf("input = %v, want %v", req.Input, texts)
		}
		writeVectors(w, req.Input, testDims)
	})

	e, rec := newTestEmbedder(t, ts.URL, testDims)
	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	assertVectors(t, got, texts, testDims)
	if n := ts.requests(); n != 1 {
		t.Errorf("requests = %d, want 1 (the whole batch travels in one call)", n)
	}
	if waits := rec.recorded(); len(waits) != 0 {
		t.Errorf("waits = %v, want none", waits)
	}
}

func TestEmbedRejectsVectorCountMismatch(t *testing.T) {
	texts := []string{"alpha", "beta", "gamma"}
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, req request) {
		writeVectors(w, req.Input[:1], testDims)
	})

	e, rec := newTestEmbedder(t, ts.URL, testDims)
	_, err := e.Embed(context.Background(), texts)
	if err == nil {
		t.Fatal("Embed succeeded, want error")
	}

	if want := "got 1 vectors for 3 inputs"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
	if n := ts.requests(); n != 1 {
		t.Errorf("requests = %d, want 1 (a protocol violation is not transient)", n)
	}
	if waits := rec.recorded(); len(waits) != 0 {
		t.Errorf("waits = %v, want none", waits)
	}
}

func TestEmbedRejectsDimensionMismatch(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		answered   int
	}{
		{name: "narrower than configured", configured: testDims, answered: testDims - 1},
		{name: "wider than configured", configured: 512, answered: 768},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, _ int, req request) {
				writeVectors(w, req.Input, tc.answered)
			})

			e, rec := newTestEmbedder(t, ts.URL, tc.configured)
			_, err := e.Embed(context.Background(), []string{"alpha", "beta"})
			if err == nil {
				t.Fatal("Embed succeeded, want error: storing a wrong-width vector corrupts the index")
			}

			want := fmt.Sprintf("returned %d dimensions for input 0, want %d", tc.answered, tc.configured)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
			if !strings.Contains(err.Error(), testModel) {
				t.Errorf("error %q does not name the model", err)
			}
			if n := ts.requests(); n != 1 {
				t.Errorf("requests = %d, want 1 (a wrong width is not transient)", n)
			}
			if waits := rec.recorded(); len(waits) != 0 {
				t.Errorf("waits = %v, want none", waits)
			}
		})
	}
}

func TestEmbedSurfacesDaemonErrorUnderA200(t *testing.T) {
	const daemonMessage = "unable to load model"
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"` + daemonMessage + `"}`))
	})

	e, _ := newTestEmbedder(t, ts.URL, testDims)
	_, err := e.Embed(context.Background(), []string{"alpha"})
	if err == nil {
		t.Fatal("Embed succeeded, want error")
	}
	if !strings.Contains(err.Error(), daemonMessage) {
		t.Errorf("error %q does not carry the daemon's stated reason", err)
	}
}

func TestEmbedSurfacesDaemonErrorForUnknownModel(t *testing.T) {
	const daemonMessage = `model "nomic-embed-text" not found, try pulling it first`
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"` + daemonMessage + `"}`))
	})

	e, rec := newTestEmbedder(t, ts.URL, testDims)
	_, err := e.Embed(context.Background(), []string{"alpha"})
	if err == nil {
		t.Fatal("Embed succeeded, want error")
	}

	for _, want := range []string{"404", daemonMessage} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if n := ts.requests(); n != 1 {
		t.Errorf("requests = %d, want 1 (a missing model is not retried)", n)
	}
	if waits := rec.recorded(); len(waits) != 0 {
		t.Errorf("waits = %v, want none", waits)
	}
}

func TestEmbedRetriesADeadDaemonThenReports(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
		t.Error("server called, want a refused connection")
		w.WriteHeader(http.StatusInternalServerError)
	})
	url := ts.URL
	ts.Close()

	e, rec := newTestEmbedder(t, url, testDims)
	_, err := e.Embed(context.Background(), []string{"alpha"})
	if err == nil {
		t.Fatal("Embed succeeded, want error")
	}

	if want := fmt.Sprintf("after %d attempts", httpretry.MaxAttempts); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
	if waits := rec.recorded(); len(waits) != httpretry.MaxAttempts-1 {
		t.Errorf("waits = %v, want %d entries", waits, httpretry.MaxAttempts-1)
	}
}

func TestEmbedRespectsContextCancel(t *testing.T) {
	t.Run("cancelled before the call", func(t *testing.T) {
		ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
			t.Error("server called, want no request for a cancelled context")
			w.WriteHeader(http.StatusInternalServerError)
		})
		e, _ := newTestEmbedder(t, ts.URL, testDims)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := e.Embed(ctx, []string{"alpha"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Embed error = %v, want context.Canceled", err)
		}
	})

	t.Run("cancelled during backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			cancel()
		})

		// The real sleep, not the recorder: this is the wait being interrupted.
		e, err := New(testModel, ts.URL, testDims)
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
	})
}

func TestIdentityNeedsNoRequest(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ int, _ request) {
		t.Error("server called, want Identity to answer offline")
		w.WriteHeader(http.StatusInternalServerError)
	})
	e, _ := newTestEmbedder(t, ts.URL, 768)

	const want = "ollama/nomic-embed-text/768"
	if got := e.Identity(); got != want {
		t.Fatalf("Identity = %q, want %q", got, want)
	}
	if again := e.Identity(); again != want {
		t.Errorf("Identity (second call) = %q, want %q", again, want)
	}
	if n := ts.requests(); n != 0 {
		t.Errorf("requests = %d, want none", n)
	}

	// Every component is load-bearing: a different model or width is a
	// different vector space, and must not read as the same identity.
	narrower, _ := newTestEmbedder(t, ts.URL, 256)
	if narrower.Identity() == want {
		t.Errorf("Identity for 256 dims = %q, want a distinct value", narrower.Identity())
	}
	other, err := New("mxbai-embed-large", "", 768)
	if err != nil {
		t.Fatalf("New (other model): %v", err)
	}
	if other.Identity() == want {
		t.Errorf("Identity for another model = %q, want a distinct value", other.Identity())
	}
}

func TestNewValidatesArguments(t *testing.T) {
	cases := []struct {
		name  string
		model string
		dims  int
	}{
		{name: "no model", dims: testDims},
		{name: "zero dims", model: testModel},
		{name: "negative dims", model: testModel, dims: -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.model, "", tc.dims); err == nil {
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
		{baseURL: "", want: "http://127.0.0.1:11434" + embedPath},
		{baseURL: "http://gpu-box.internal:11434", want: "http://gpu-box.internal:11434" + embedPath},
		{baseURL: "http://gpu-box.internal:11434/", want: "http://gpu-box.internal:11434" + embedPath},
	}

	for _, tc := range cases {
		e, err := New(testModel, tc.baseURL, testDims)
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

	if _, err := e.Embed(ctx, []string{"alpha", ""}); err == nil {
		t.Error("Embed with an empty text succeeded, want error")
	} else if !strings.Contains(err.Error(), "texts[1]") {
		t.Errorf("error %q does not name the offending index", err)
	}
}
