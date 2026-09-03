package zai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/connectors/httpretry/httpretrytest"
	"github.com/setthasit/Lore/internal/connectors/llm/openai"
)

const (
	// An obviously fake credential: these tests must never need a real one, and
	// one of them asserts this exact string never reaches an error message.
	fakeKey    = "fake-zai-key.fake-secret"
	testModel  = "glm-fake"
	testSystem = "You cite sources."
	testUser   = "Why was the cache added?"

	chinaBaseURL = "https://open.bigmodel.cn/api"
)

// Retry, timeout and error-scrubbing behaviour belongs to the OpenAI chat
// completions implementation and is tested there; these tests pin what Z.AI
// changes: the endpoint it is reached at.
type recordingTransport struct {
	url  string
	body []byte
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.url = r.URL.String()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	rt.body = body

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)),
		Request:    r,
	}, nil
}

func TestNewResolvesEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "default", want: DefaultBaseURL + chatPath},
		{name: "china deployment", baseURL: chinaBaseURL, want: chinaBaseURL + chatPath},
		{name: "trailing slash", baseURL: chinaBaseURL + "/", want: chinaBaseURL + chatPath},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &recordingTransport{}
			c, err := New(fakeKey, testModel, tc.baseURL, openai.WithHTTPClient(&http.Client{Transport: rt}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := c.Complete(context.Background(), testSystem, testUser); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if rt.url != tc.want {
				t.Errorf("url = %q, want %q", rt.url, tc.want)
			}
		})
	}
}

func TestCompleteSpeaksChatCompletions(t *testing.T) {
	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Path != chatPath {
			t.Errorf("path = %q, want %q", r.URL.Path, chatPath)
		}
		if want := "Bearer " + fakeKey; r.Header.Get("Authorization") != want {
			t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), want)
		}

		var got struct {
			Model    string `json:"model"`
			Stream   *bool  `json:"stream"`
			System   string `json:"system"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}

		if got.Model != testModel {
			t.Errorf("model = %q, want %q", got.Model, testModel)
		}
		if got.Stream == nil || *got.Stream {
			t.Errorf("stream = %v, want an explicit false", got.Stream)
		}
		// GLM takes the system prompt as a message role, not a top-level field.
		if got.System != "" {
			t.Errorf("top-level system = %q, want it sent as a message", got.System)
		}
		if len(got.Messages) != 2 {
			t.Errorf("messages = %+v, want system then user", got.Messages)
		} else {
			if got.Messages[0].Role != "system" || got.Messages[0].Content != testSystem {
				t.Errorf("messages[0] = %+v, want system %q", got.Messages[0], testSystem)
			}
			if got.Messages[1].Role != "user" || got.Messages[1].Content != testUser {
				t.Errorf("messages[1] = %+v, want user %q", got.Messages[1], testUser)
			}
		}

		httpretrytest.WriteJSON(w, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"Because reads dominated."}}]}`)
	})

	c, err := New(fakeKey, testModel, ts.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text, err := c.Complete(context.Background(), testSystem, testUser)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := "Because reads dominated."; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

func TestCompleteFailsFastOnClientError(t *testing.T) {
	// The provider echoes the credential it rejected; the error built from that
	// body is what reaches logs and user output.
	echoBody := fmt.Sprintf(`{"error":{"code":"1002","message":"invalid api key: %s"}}`, fakeKey)

	ts := httpretrytest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		httpretrytest.WriteJSON(w, http.StatusUnauthorized, echoBody)
	})

	c, err := New(fakeKey, testModel, ts.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Complete(context.Background(), testSystem, testUser)
	if err == nil {
		t.Fatal("Complete succeeded, want error")
	}
	if n := ts.Attempts(); n != 1 {
		t.Errorf("attempts = %d, want 1 (a 4xx other than 429 is permanent)", n)
	}
	if !strings.Contains(err.Error(), "zai") {
		t.Errorf("error %q does not name the provider", err)
	}
	if strings.Contains(err.Error(), fakeKey) {
		t.Errorf("error %q leaks the api key", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("error %q does not mark the redaction", err)
	}
}

func TestNewValidatesArguments(t *testing.T) {
	if _, err := New("", testModel, ""); err == nil {
		t.Error("New without an api key succeeded, want error")
	} else if !strings.Contains(err.Error(), "zai") {
		t.Errorf("error %q does not name the provider", err)
	}
	if _, err := New(fakeKey, "", ""); err == nil {
		t.Error("New without a model succeeded, want error")
	}
}
