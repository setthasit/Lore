package openai

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

	"github.com/setthasit/Lore/sdk/httpx"
	"github.com/setthasit/Lore/sdk/httpx/httpxtest"
)

const (
	// An obviously fake credential: these tests must never need a real one, and
	// several of them assert this exact string never reaches an error message.
	fakeKey    = "sk-fake-test-key"
	testModel  = "gpt-fake-mini"
	testSystem = "You cite sources."
	testUser   = "Why was the cache added?"
)

func newTestClient(t *testing.T, baseURL string) (*Client, *httpxtest.WaitRecorder) {
	t.Helper()

	c, err := New(fakeKey, testModel, baseURL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := &httpxtest.WaitRecorder{}
	c.call.Sleep = rec.Sleep
	return c, rec
}

func answer(text string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q}}]}`, text)
}

func TestCompleteSendsChatCompletionsRequest(t *testing.T) {
	var got struct {
		Model    string `json:"model"`
		Stream   *bool  `json:"stream"`
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	ts := httpxtest.NewServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want %q", r.Method, http.MethodPost)
		}
		if r.URL.Path != chatPath {
			t.Errorf("path = %q, want %q", r.URL.Path, chatPath)
		}
		if want := "Bearer " + fakeKey; r.Header.Get("Authorization") != want {
			t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), want)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		httpxtest.WriteJSON(w, http.StatusOK, answer("Because reads dominated."))
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
	if got.Stream == nil || *got.Stream {
		t.Errorf("stream = %v, want an explicit false", got.Stream)
	}
	// Sending the system prompt as a top-level field silently drops it here.
	if got.System != "" {
		t.Errorf("top-level system = %q, want it sent as a message", got.System)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %v, want system then user", got.Messages)
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
	ts := httpxtest.NewServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		httpxtest.WriteJSON(w, http.StatusOK, answer("ok"))
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
	ts := httpxtest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
		if attempt == 1 {
			w.Header().Set("Retry-After", "2")
			httpxtest.WriteJSON(w, http.StatusTooManyRequests, `{"error":{"message":"Rate limit reached","code":"rate_limit_exceeded"}}`)
			return
		}
		httpxtest.WriteJSON(w, http.StatusOK, answer("second try"))
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
	ts := httpxtest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
		if attempt == 1 {
			http.Error(w, "upstream exploded", http.StatusBadGateway)
			return
		}
		httpxtest.WriteJSON(w, http.StatusOK, answer("recovered"))
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
	waits := rec.Recorded()
	if len(waits) != 1 {
		t.Fatalf("waits = %v, want one entry", waits)
	}
	if waits[0] <= 0 || waits[0] > time.Second {
		t.Errorf("wait = %v, want a jittered sub-second backoff", waits[0])
	}
}

func TestCompleteFailsFastOnClientError(t *testing.T) {
	ts := httpxtest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		httpxtest.WriteJSON(w, http.StatusBadRequest, `{"error":{"message":"'messages' is invalid","code":"invalid_request_error"}}`)
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
	for _, want := range []string{"400", "invalid_request_error", "'messages' is invalid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestCompleteRejectsAnswerlessResponses(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantErr     string
		wantRetries bool
	}{
		{
			name:    "no choices",
			body:    `{"choices":[]}`,
			wantErr: "answered with no text",
		},
		{
			name:    "empty content",
			body:    `{"choices":[{"message":{"role":"assistant","content":""}}]}`,
			wantErr: "answered with no text",
		},
		{
			name:        "malformed json",
			body:        `{"choices":[{"message":`,
			wantErr:     "decode response",
			wantRetries: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httpxtest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				httpxtest.WriteJSON(w, http.StatusOK, tc.body)
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
				wantAttempts = httpx.MaxAttempts
			}
			if n := ts.Attempts(); n != wantAttempts {
				t.Errorf("attempts = %d, want %d", n, wantAttempts)
			}
		})
	}
}

func TestCompleteStopsAfterAttemptBudget(t *testing.T) {
	ts := httpxtest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	})

	c, rec := newTestClient(t, ts.URL)
	_, err := c.Complete(context.Background(), testSystem, testUser)
	if err == nil {
		t.Fatal("Complete succeeded, want error")
	}
	if n := ts.Attempts(); n != httpx.MaxAttempts {
		t.Errorf("attempts = %d, want %d", n, httpx.MaxAttempts)
	}
	if waits := rec.Recorded(); len(waits) != httpx.MaxAttempts-1 {
		t.Errorf("waits = %v, want %d entries", waits, httpx.MaxAttempts-1)
	}
	for _, want := range []string{fmt.Sprintf("after %d attempts", httpx.MaxAttempts), "503"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestCompleteRespectsContextCancellation(t *testing.T) {
	t.Run("before the request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		ts := httpxtest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			t.Error("server called, want no request")
			httpxtest.WriteJSON(w, http.StatusOK, answer("unreachable"))
		})

		c, _ := newTestClient(t, ts.URL)
		if _, err := c.Complete(ctx, testSystem, testUser); !errors.Is(err, context.Canceled) {
			t.Fatalf("Complete error = %v, want context.Canceled", err)
		}
	})

	t.Run("during the retry wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ts := httpxtest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set("Retry-After", "600")
			httpxtest.WriteJSON(w, http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`)
		})

		c, err := New(fakeKey, testModel, ts.URL)
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

func TestCompleteErrorsOmitAPIKey(t *testing.T) {
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
			ts := httpxtest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				httpxtest.WriteJSON(w, tc.status, echoBody)
			})

			c, _ := newTestClient(t, ts.URL)
			if _, err := c.Complete(context.Background(), testSystem, testUser); err == nil {
				t.Fatal("Complete succeeded, want error")
			} else if strings.Contains(err.Error(), fakeKey) {
				t.Errorf("error %q leaks the api key", err)
			} else if !strings.Contains(err.Error(), "[redacted]") {
				t.Errorf("error %q does not mark the redaction", err)
			}
		})
	}
}

func TestNewValidatesChatArguments(t *testing.T) {
	if _, err := New("", testModel, ""); err == nil {
		t.Error("New without an api key succeeded, want error")
	}
	if _, err := New(fakeKey, "", ""); err == nil {
		t.Error("New without a model succeeded, want error")
	}

	c, err := New(fakeKey, testModel, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Complete(context.Background(), testSystem, ""); err == nil {
		t.Error("Complete without a user prompt succeeded, want error")
	}
}

func TestNewResolvesChatEndpoint(t *testing.T) {
	cases := []struct {
		baseURL string
		want    string
	}{
		{baseURL: "", want: DefaultBaseURL + chatPath},
		{baseURL: "https://gateway.internal", want: "https://gateway.internal" + chatPath},
		{baseURL: "https://gateway.internal/", want: "https://gateway.internal" + chatPath},
	}

	for _, tc := range cases {
		c, err := New(fakeKey, testModel, tc.baseURL)
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
	c, err := New(fakeKey, testModel, "", WithHTTPClient(custom))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.call.HTTP != custom {
		t.Error("WithHTTPClient did not install the client")
	}
	if _, err := New(fakeKey, testModel, "", WithHTTPClient(nil)); err != nil {
		t.Fatalf("New with a nil client override: %v", err)
	}
}
