package httpretry

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/connectors/httpretry/httpretrytest"
)

type chatBody struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func TestPostJSONWritesOutOnlyOnASoundBody(t *testing.T) {
	// json.Decoder fills earlier fields before reporting a type error, so a
	// half-decoded attempt must never be retried into a thinner second body.
	const partlyTyped = `{"choices":[{"message":{"content":"stale"}},{"message":{"content":5}}]}`

	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
		if attempt == 1 {
			httpretrytest.WriteJSON(w, http.StatusOK, partlyTyped)
			return
		}
		httpretrytest.WriteJSON(w, http.StatusOK, `{}`)
	})

	rec := &httpretrytest.WaitRecorder{}
	call := Client{Sleep: rec.Sleep, Op: "test: chat"}

	var out chatBody
	err := call.PostJSON(context.Background(), ts.URL, nil, []byte(`{}`), &out)
	if err == nil {
		t.Fatalf("PostJSON succeeded with out = %+v, wanted the type error reported", out)
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("error %q does not name the decode failure", err)
	}
	if n := ts.Attempts(); n != 1 {
		t.Errorf("attempts = %d, want 1: a well-formed body of the wrong type is permanent", n)
	}
}

func TestPostJSONRetriesATruncatedBody(t *testing.T) {
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
		if attempt < MaxAttempts {
			httpretrytest.WriteJSON(w, http.StatusOK, `{"choices":[{"message":`)
			return
		}
		httpretrytest.WriteJSON(w, http.StatusOK, `{"choices":[{"message":{"content":"whole"}}]}`)
	})

	rec := &httpretrytest.WaitRecorder{}
	call := Client{Sleep: rec.Sleep, Op: "test: chat"}

	var out chatBody
	if err := call.PostJSON(context.Background(), ts.URL, nil, []byte(`{}`), &out); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "whole" {
		t.Errorf("out = %+v, want the last body only", out)
	}
	if n := ts.Attempts(); n != MaxAttempts {
		t.Errorf("attempts = %d, want %d", n, MaxAttempts)
	}
}

func TestWaitAbandonsTheDelayOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if err := (Client{}).wait(ctx, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want the delay abandoned when the context ended", elapsed)
	}
}

func TestRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{name: "absent", header: http.Header{}},
		{name: "delta seconds", header: http.Header{"Retry-After": {"30"}}, want: 30 * time.Second},
		{name: "zero", header: http.Header{"Retry-After": {"0"}}},
		{name: "negative", header: http.Header{"Retry-After": {"-5"}}},
		{name: "unparseable", header: http.Header{"Retry-After": {"soon"}}},
		{
			name:   "http date in the past",
			header: http.Header{"Retry-After": {time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfter(tc.header); got != tc.want {
				t.Errorf("retryAfter = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("http date in the future", func(t *testing.T) {
		header := http.Header{"Retry-After": {time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)}}
		// The date resolves against the wall clock, so only the bound is stable.
		if got := retryAfter(header); got <= 25*time.Second || got > 30*time.Second {
			t.Errorf("retryAfter = %v, want just under 30s", got)
		}
	})
}

func TestBackoff(t *testing.T) {
	t.Run("honors a server directive", func(t *testing.T) {
		if got := backoff(1, 7*time.Second); got != 7*time.Second {
			t.Errorf("backoff = %v, want the requested 7s", got)
		}
	})

	t.Run("caps a server directive", func(t *testing.T) {
		if got := backoff(1, time.Hour); got != maxRetryAfter {
			t.Errorf("backoff = %v, want the %v cap", got, maxRetryAfter)
		}
	})

	t.Run("grows the jittered window", func(t *testing.T) {
		for attempt := 1; attempt <= 8; attempt++ {
			window := min(BaseBackoff<<(attempt-1), maxBackoff)
			got := backoff(attempt, 0)
			if got < window/2 || got > window {
				t.Errorf("backoff(attempt %d) = %v, want within [%v, %v]", attempt, got, window/2, window)
			}
		}
	})
}

func TestReadableRedactsOnlyARealSecret(t *testing.T) {
	const key = "sk-fake-test-key"

	got := Client{Secret: key}.readable("rejected " + key)
	if want := "rejected " + redaction; got != want {
		t.Errorf("readable = %q, want %q", got, want)
	}

	// An unauthenticated provider has no secret; replacing an empty string
	// would splice the redaction marker between every character.
	if got := (Client{}).readable("model not found"); got != "model not found" {
		t.Errorf("readable without a secret = %q, want the message unchanged", got)
	}
}

func TestReadableTruncatesLongMessages(t *testing.T) {
	got := Client{}.readable(strings.Repeat("a", messageLimit*2))
	if len(got) <= messageLimit || !strings.HasSuffix(got, "…") {
		t.Errorf("readable produced %d bytes ending %q, want a truncation marker", len(got), got[max(len(got)-4, 0):])
	}
}

func TestAPIError(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "bare string", raw: `"model not found"`, want: "model not found"},
		{
			name: "message with a code",
			raw:  `{"message":"Rate limit reached","code":"rate_limit_exceeded"}`,
			want: "rate_limit_exceeded: Rate limit reached",
		},
		{
			name: "message with a type",
			raw:  `{"message":"credit balance is too low","type":"invalid_request_error"}`,
			want: "invalid_request_error: credit balance is too low",
		},
		{name: "message only", raw: `{"message":"slow down"}`, want: "slow down"},
		{name: "no message", raw: `{"status":503}`},
		{name: "absent", raw: ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := apiError([]byte(tc.raw)); got != tc.want {
				t.Errorf("apiError = %q, want %q", got, tc.want)
			}
		})
	}
}
