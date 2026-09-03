package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/connectors/httpretry"
	"github.com/setthasit/Lore/internal/connectors/httpretry/httpretrytest"
)

const (
	// An obviously fake credential: these tests must never need a real one, and
	// several of them assert this exact string never reaches an error message.
	fakeKey    = "sk-ant-fake-test-key"
	testModel  = "claude-fake-4"
	testSystem = "You cite sources."
	testUser   = "Why was the cache added?"
)

func newTestClient(t *testing.T, baseURL string) (*Client, *httpretrytest.WaitRecorder) {
	t.Helper()

	c, err := New(fakeKey, testModel, baseURL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := &httpretrytest.WaitRecorder{}
	c.call.Sleep = rec.Sleep
	return c, rec
}

func answer(text string) string {
	return fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, text)
}

func TestCompleteSendsMessagesRequest(t *testing.T) {
	var got struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want %q", r.Method, http.MethodPost)
		}
		if r.URL.Path != messagesPath {
			t.Errorf("path = %q, want %q", r.URL.Path, messagesPath)
		}
		if key := r.Header.Get("x-api-key"); key != fakeKey {
			t.Errorf("x-api-key = %q, want the credential", key)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("Authorization = %q, want the key in x-api-key only", auth)
		}
		if version := r.Header.Get("anthropic-version"); version != apiVersion {
			t.Errorf("anthropic-version = %q, want %q", version, apiVersion)
		}
		if ct := r.Header.Get("content-type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
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
	// Omitting max_tokens is a 400 here, so its presence is the contract.
	if got.MaxTokens != maxTokens {
		t.Errorf("max_tokens = %d, want %d", got.MaxTokens, maxTokens)
	}
	// Sending the system prompt as a message role would demote it to user text.
	if got.System != testSystem {
		t.Errorf("system = %q, want %q", got.System, testSystem)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %+v, want only the user turn", got.Messages)
	}
	if got.Messages[0].Role != "user" || got.Messages[0].Content != testUser {
		t.Errorf("messages[0] = %+v, want user %q", got.Messages[0], testUser)
	}
}

func TestCompleteOmitsEmptySystemPrompt(t *testing.T) {
	var got map[string]any
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
	if _, present := got["system"]; present {
		t.Errorf("request carries system = %v, want the field omitted", got["system"])
	}
}

func TestCompleteReadsTextBlocksOnly(t *testing.T) {
	body := `{"content":[
		{"type":"thinking","thinking":"weighing the commits"},
		{"type":"text","text":"Reads dominated"},
		{"type":"text","text":", so a cache paid off."}
	]}`
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		httpretrytest.WriteJSON(w, http.StatusOK, body)
	})

	c, _ := newTestClient(t, ts.URL)
	text, err := c.Complete(context.Background(), testSystem, testUser)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := "Reads dominated, so a cache paid off."; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

func TestCompleteRetriesRateLimit(t *testing.T) {
	cases := []struct {
		name       string
		retryAfter string
		wantWait   func(time.Duration) bool
	}{
		{
			name:       "with retry-after",
			retryAfter: "2",
			wantWait:   func(d time.Duration) bool { return d == 2*time.Second },
		},
		{
			// The spend-cap variant of 429 carries no retry-after.
			name:     "without retry-after",
			wantWait: func(d time.Duration) bool { return d > 0 && d <= time.Second },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
				if attempt == 1 {
					if tc.retryAfter != "" {
						w.Header().Set("Retry-After", tc.retryAfter)
					}
					httpretrytest.WriteJSON(w, http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
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
			waits := rec.Recorded()
			if len(waits) != 1 || !tc.wantWait(waits[0]) {
				t.Errorf("waits = %v, want one acceptable delay", waits)
			}
		})
	}
}

func TestCompleteRetriesServerError(t *testing.T) {
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
		if attempt == 1 {
			httpretrytest.WriteJSON(w, http.StatusInternalServerError, `{"type":"error","error":{"type":"api_error","message":"internal"}}`)
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
		httpretrytest.WriteJSON(w, http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: field required"}}`)
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
	for _, want := range []string{"400", "invalid_request_error", "max_tokens: field required"} {
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
		{name: "no blocks", body: `{"content":[]}`, wantErr: "no text block"},
		{
			name:    "no text block",
			body:    `{"content":[{"type":"thinking","thinking":"…"}]}`,
			wantErr: "no text block",
		},
		{
			name:    "empty text",
			body:    `{"content":[{"type":"text","text":""}]}`,
			wantErr: "no text block",
		},
		{
			name:        "malformed json",
			body:        `{"content":[{"type":`,
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

func TestCompleteStopsAfterAttemptBudget(t *testing.T) {
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		httpretrytest.WriteJSON(w, http.StatusServiceUnavailable, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
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
			httpretrytest.WriteJSON(w, http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
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

func TestErrorsOmitAPIKey(t *testing.T) {
	// The provider echoes the credential it rejected; the error built from that
	// body is what reaches logs and user output.
	echoBody := fmt.Sprintf(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key: %s"}}`, fakeKey)

	cases := []struct {
		name   string
		status int
	}{
		{name: "client error", status: http.StatusUnauthorized},
		{name: "retried until exhausted", status: http.StatusTooManyRequests},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				httpretrytest.WriteJSON(w, tc.status, echoBody)
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

func TestNewValidatesArguments(t *testing.T) {
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

func TestNewResolvesEndpoint(t *testing.T) {
	cases := []struct {
		baseURL string
		want    string
	}{
		{baseURL: "", want: DefaultBaseURL + messagesPath},
		{baseURL: "https://gateway.internal", want: "https://gateway.internal" + messagesPath},
		{baseURL: "https://gateway.internal/", want: "https://gateway.internal" + messagesPath},
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
}
