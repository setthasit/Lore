package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/connectors/httpretry"
	"github.com/setthasit/Lore/internal/connectors/httpretry/httpretrytest"
)

const (
	testModel  = "llama-fake"
	testSystem = "You cite sources."
	testUser   = "Why was the cache added?"
)

func newTestClient(t *testing.T, baseURL string) (*Client, *httpretrytest.WaitRecorder) {
	t.Helper()

	c, err := New(testModel, baseURL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := &httpretrytest.WaitRecorder{}
	c.call.Sleep = rec.Sleep
	return c, rec
}

func answer(text string) string {
	return fmt.Sprintf(`{"model":%q,"message":{"role":"assistant","content":%q},"done":true}`, testModel, text)
}

func TestCompleteSendsChatRequest(t *testing.T) {
	var got struct {
		Model    string `json:"model"`
		Stream   *bool  `json:"stream"`
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want %q", r.Method, http.MethodPost)
		}
		if r.URL.Path != chatPath {
			t.Errorf("path = %q, want %q", r.URL.Path, chatPath)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("Authorization = %q, want none: the daemon is unauthenticated", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		httpretrytest.WriteJSON(w, http.StatusOK, answer("Because reads dominated."))
	})

	c, _ := newTestClient(t, ts.URL)
	text, err := c.Complete(context.Background(), testSystem, testUser)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := "Because reads dominated."; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}

	if got.Model != testModel {
		t.Errorf("model = %q, want %q", got.Model, testModel)
	}
	// Without an explicit false the daemon answers with an NDJSON stream.
	if got.Stream == nil || *got.Stream {
		t.Errorf("stream = %v, want an explicit false", got.Stream)
	}
	// The system prompt is a message role here, not a top-level field.
	if got.System != "" {
		t.Errorf("top-level system = %q, want it sent as a message", got.System)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %+v, want system then user", got.Messages)
	}
	if got.Messages[0].Role != "system" || got.Messages[0].Content != testSystem {
		t.Errorf("messages[0] = %+v, want system %q", got.Messages[0], testSystem)
	}
	if got.Messages[1].Role != "user" || got.Messages[1].Content != testUser {
		t.Errorf("messages[1] = %+v, want user %q", got.Messages[1], testUser)
	}
}

func TestCompleteOmitsEmptySystemPrompt(t *testing.T) {
	var got struct {
		Messages []message `json:"messages"`
	}
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		httpretrytest.WriteJSON(w, http.StatusOK, answer("ok"))
	})

	c, _ := newTestClient(t, ts.URL)
	if _, err := c.Complete(context.Background(), "", testUser); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Errorf("messages = %+v, want only the user turn", got.Messages)
	}
}

func TestCompleteRetriesRateLimitHonoringRetryAfter(t *testing.T) {
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
		if attempt == 1 {
			w.Header().Set("Retry-After", "2")
			httpretrytest.WriteJSON(w, http.StatusTooManyRequests, `{"error":"too many requests"}`)
			return
		}
		httpretrytest.WriteJSON(w, http.StatusOK, answer("second try"))
	})

	c, rec := newTestClient(t, ts.URL)
	text, err := c.Complete(context.Background(), testSystem, testUser)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if text != "second try" {
		t.Errorf("text = %q, want %q", text, "second try")
	}
	if n := ts.Attempts(); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
	if waits := rec.Recorded(); !slices.Equal(waits, []time.Duration{2 * time.Second}) {
		t.Errorf("waits = %v, want [2s] from Retry-After", waits)
	}
}

func TestCompleteRetriesServerError(t *testing.T) {
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
		if attempt == 1 {
			httpretrytest.WriteJSON(w, http.StatusInternalServerError, `{"error":"llama runner process has terminated"}`)
			return
		}
		httpretrytest.WriteJSON(w, http.StatusOK, answer("recovered"))
	})

	c, rec := newTestClient(t, ts.URL)
	text, err := c.Complete(context.Background(), testSystem, testUser)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if text != "recovered" {
		t.Errorf("text = %q, want %q", text, "recovered")
	}
	if n := ts.Attempts(); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
	if waits := rec.Recorded(); len(waits) != 1 {
		t.Errorf("waits = %v, want one entry", waits)
	}
}

func TestCompleteFailsFastOnClientError(t *testing.T) {
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		httpretrytest.WriteJSON(w, http.StatusBadRequest, `{"error":"model 'llama-fake' not found"}`)
	})

	c, rec := newTestClient(t, ts.URL)
	_, err := c.Complete(context.Background(), testSystem, testUser)
	if err == nil {
		t.Fatal("Complete succeeded, want error")
	}
	if n := ts.Attempts(); n != 1 {
		t.Errorf("attempts = %d, want 1 (a 4xx other than 429 is permanent)", n)
	}
	if waits := rec.Recorded(); len(waits) != 0 {
		t.Errorf("waits = %v, want none", waits)
	}
	// The daemon states failures as a bare {"error":"…"} string.
	for _, want := range []string{"400", "model 'llama-fake' not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), testUser) {
		t.Errorf("error %q repeats the request body", err)
	}
}

func TestCompleteRejectsAnswerlessResponses(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantErr     string
		wantRetries bool
	}{
		{name: "no message", body: `{"done":true}`, wantErr: "answered with no text"},
		{
			name:    "empty content",
			body:    `{"message":{"role":"assistant","content":""}}`,
			wantErr: "answered with no text",
		},
		{
			// The daemon answers with one message object; a choices array is a
			// different provider's shape and carries no answer here.
			name:    "openai shaped body",
			body:    `{"choices":[{"message":{"role":"assistant","content":"wrong shape"}}]}`,
			wantErr: "answered with no text",
		},
		{
			name:        "malformed json",
			body:        `{"message":{"role":`,
			wantErr:     "decode response",
			wantRetries: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				httpretrytest.WriteJSON(w, http.StatusOK, tc.body)
			})

			c, _ := newTestClient(t, ts.URL)
			text, err := c.Complete(context.Background(), testSystem, testUser)
			if err == nil {
				t.Fatalf("Complete = %q, want error", text)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}

			wantAttempts := 1
			if tc.wantRetries {
				wantAttempts = httpretry.MaxAttempts
			}
			if n := ts.Attempts(); n != wantAttempts {
				t.Errorf("attempts = %d, want %d", n, wantAttempts)
			}
		})
	}
}

func TestCompleteSurfacesDaemonErrorUnderOK(t *testing.T) {
	const daemonMessage = "model requires more system memory than is available"

	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		httpretrytest.WriteJSON(w, http.StatusOK, fmt.Sprintf(`{"error":%q}`, daemonMessage))
	})

	c, _ := newTestClient(t, ts.URL)
	_, err := c.Complete(context.Background(), testSystem, testUser)
	if err == nil {
		t.Fatal("Complete succeeded, want error")
	}
	if !strings.Contains(err.Error(), daemonMessage) {
		t.Errorf("error %q drops the daemon's stated reason", err)
	}
	if strings.Contains(err.Error(), "answered with no text") {
		t.Errorf("error %q reports an empty answer instead of the refusal", err)
	}
	if n := ts.Attempts(); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestCompleteStopsAfterAttemptBudget(t *testing.T) {
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		httpretrytest.WriteJSON(w, http.StatusServiceUnavailable, `{"error":"server busy"}`)
	})

	c, rec := newTestClient(t, ts.URL)
	_, err := c.Complete(context.Background(), testSystem, testUser)
	if err == nil {
		t.Fatal("Complete succeeded, want error")
	}
	if n := ts.Attempts(); n != httpretry.MaxAttempts {
		t.Errorf("attempts = %d, want %d", n, httpretry.MaxAttempts)
	}
	if waits := rec.Recorded(); len(waits) != httpretry.MaxAttempts-1 {
		t.Errorf("waits = %v, want %d entries", waits, httpretry.MaxAttempts-1)
	}
	for _, want := range []string{fmt.Sprintf("after %d attempts", httpretry.MaxAttempts), "503"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestCompleteReportsUnreachableDaemon(t *testing.T) {
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		httpretrytest.WriteJSON(w, http.StatusOK, answer("unreachable"))
	})
	url := ts.URL
	ts.Close()

	c, rec := newTestClient(t, url)
	_, err := c.Complete(context.Background(), testSystem, testUser)
	if err == nil {
		t.Fatal("Complete succeeded, want error")
	}
	if !strings.Contains(err.Error(), "ollama: chat") {
		t.Errorf("error %q does not name the failing call", err)
	}
	// A dead local daemon may be mid-restart, so the transport error is retried.
	if waits := rec.Recorded(); len(waits) != httpretry.MaxAttempts-1 {
		t.Errorf("waits = %v, want %d entries", waits, httpretry.MaxAttempts-1)
	}
}

func TestCompleteRespectsContextCancellation(t *testing.T) {
	t.Run("before the request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			t.Error("server called, want no request")
			httpretrytest.WriteJSON(w, http.StatusOK, answer("unreachable"))
		})

		c, _ := newTestClient(t, ts.URL)
		if _, err := c.Complete(ctx, testSystem, testUser); !errors.Is(err, context.Canceled) {
			t.Fatalf("Complete error = %v, want context.Canceled", err)
		}
	})

	t.Run("during the retry wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set("Retry-After", "600")
			httpretrytest.WriteJSON(w, http.StatusTooManyRequests, `{"error":"slow down"}`)
		})

		c, err := New(testModel, ts.URL)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		// Cancelling from inside the wait is the path a caller hits: by then the
		// 429 has been read and only the delay is left to abandon.
		c.call.Sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}

		if _, err := c.Complete(ctx, testSystem, testUser); !errors.Is(err, context.Canceled) {
			t.Fatalf("Complete error = %v, want context.Canceled", err)
		}
		if n := ts.Attempts(); n != 1 {
			t.Errorf("attempts = %d, want 1", n)
		}
	})
}

func TestNewValidatesArguments(t *testing.T) {
	if _, err := New("", ""); err == nil {
		t.Error("New without a model succeeded, want error")
	}

	c, err := New(testModel, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Complete(context.Background(), testSystem, ""); err == nil {
		t.Error("Complete without a user prompt succeeded, want error")
	}
}

func TestNewResolvesEndpoint(t *testing.T) {
	cases := []struct {
		baseURL string
		want    string
	}{
		{baseURL: "", want: DefaultBaseURL + chatPath},
		{baseURL: "http://gpu-box.internal:11434", want: "http://gpu-box.internal:11434" + chatPath},
		{baseURL: "http://gpu-box.internal:11434/", want: "http://gpu-box.internal:11434" + chatPath},
	}

	for _, tc := range cases {
		c, err := New(testModel, tc.baseURL)
		if err != nil {
			t.Fatalf("New(%q): %v", tc.baseURL, err)
		}
		if c.endpoint != tc.want {
			t.Errorf("endpoint for %q = %q, want %q", tc.baseURL, c.endpoint, tc.want)
		}
	}
}

func TestWithHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: time.Second}
	c, err := New(testModel, "", WithHTTPClient(custom))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.call.HTTP != custom {
		t.Error("WithHTTPClient did not install the client")
	}
}
