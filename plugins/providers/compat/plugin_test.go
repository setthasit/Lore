package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
	"github.com/setthasit/Lore/sdk/httpx/httpxtest"
)

const (
	fakeKey    = "fake-compat-key"
	testSystem = "You cite sources."
	testUser   = "Why was the cache added?"
)

func testConfig(capability lore.Capability, model, with string) lore.ProviderConfig {
	c := lore.ProviderConfig{
		Instance:   "compat",
		Capability: capability,
		Model:      model,
		Config:     json.RawMessage(with),
		Secrets:    map[string]string{"api_key": fakeKey},
	}
	if capability == lore.CapabilityEmbed {
		c.Dimensions = 1024
	}
	return c
}

func TestPluginBuildsEveryDeclaredCapability(t *testing.T) {
	conform.Provider(t, Plugin(), func(capability lore.Capability) lore.ProviderConfig {
		return testConfig(capability, "some-model", `{"preset":"together"}`)
	})
}

func TestPluginRefusesUndeclaredCapability(t *testing.T) {
	_, err := Plugin().NewProvider(testConfig("rerank", "some-model", `{"preset":"groq"}`))
	if err == nil {
		t.Fatal("NewProvider succeeded for a capability the manifest does not declare")
	}
	msg := err.Error()
	if !strings.Contains(msg, "rerank") || !strings.Contains(msg, "not served") {
		t.Errorf("error = %q, want it to name the refused capability", msg)
	}
}

func TestPluginRefusesWithoutPresetOrBaseURL(t *testing.T) {
	_, err := Plugin().NewProvider(testConfig(lore.CapabilityComplete, "some-model", `{}`))
	if err == nil {
		t.Fatal("NewProvider succeeded with neither preset nor base_url")
	}
	msg := err.Error()
	if !strings.Contains(msg, "preset") || !strings.Contains(msg, "base_url") {
		t.Errorf("error = %q, want it to offer both preset and base_url", msg)
	}
}

func TestPluginRefusesUnknownPreset(t *testing.T) {
	_, err := Plugin().NewProvider(testConfig(lore.CapabilityComplete, "some-model", `{"preset":"mistral"}`))
	if err == nil {
		t.Fatal("NewProvider succeeded for a preset the table does not hold")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mistral") || !strings.Contains(msg, "preset") {
		t.Errorf("error = %q, want it to name the unknown preset", msg)
	}
}

func TestOverridesBeatPresetDefaults(t *testing.T) {
	cases := []struct {
		name           string
		with           config
		chat           string
		embeddings     string
		defaultsPreset string
	}{
		{
			name:       "base_url overrides a preset's host",
			with:       config{Preset: "zai", BaseURL: "https://open.bigmodel.cn/api"},
			chat:       "https://open.bigmodel.cn/api/paas/v4/chat/completions",
			embeddings: "",
		},
		{
			name:       "trailing slash on an override",
			with:       config{Preset: "together", BaseURL: "https://gateway.example/"},
			chat:       "https://gateway.example/v1/chat/completions",
			embeddings: "https://gateway.example/v1/embeddings",
		},
		{
			name:       "both paths override a preset's",
			with:       config{Preset: "together", ChatPath: "/proxy/chat", EmbeddingsPath: "/proxy/vectors"},
			chat:       "https://api.together.ai/proxy/chat",
			embeddings: "https://api.together.ai/proxy/vectors",
		},
		{
			name:       "embeddings_path reaches a row that declares none",
			with:       config{Preset: "zai", EmbeddingsPath: "/paas/v4/embeddings"},
			chat:       "https://api.z.ai/api/paas/v4/chat/completions",
			embeddings: "https://api.z.ai/api/paas/v4/embeddings",
		},
		{
			name:       "base_url alone falls back to the protocol's own paths",
			with:       config{BaseURL: "https://llm.internal"},
			chat:       "https://llm.internal/v1/chat/completions",
			embeddings: "https://llm.internal/v1/embeddings",
		},
		{
			name:       "base_url with paths reaches a vendor absent from the table",
			with:       config{BaseURL: "https://api.mistral.ai", ChatPath: "/v1/chat/completions", EmbeddingsPath: "/v1/embeddings"},
			chat:       "https://api.mistral.ai/v1/chat/completions",
			embeddings: "https://api.mistral.ai/v1/embeddings",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := resolve(tc.with)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if d.chatEndpoint != tc.chat {
				t.Errorf("chat endpoint = %q, want %q", d.chatEndpoint, tc.chat)
			}
			if d.embeddingsEndpoint != tc.embeddings {
				t.Errorf("embeddings endpoint = %q, want %q", d.embeddingsEndpoint, tc.embeddings)
			}
		})
	}
}

func TestPluginRequiresDeclaredDimensions(t *testing.T) {
	cfg := testConfig(lore.CapabilityEmbed, "bge-m3", `{"preset":"vllm"}`)
	cfg.Dimensions = 0

	_, err := Plugin().NewProvider(cfg)
	if err == nil {
		t.Fatal("NewProvider built an embedder of unknown width")
	}
	msg := err.Error()
	if !strings.Contains(msg, "embedder.dimensions") || !strings.Contains(msg, "bge-m3") {
		t.Errorf("error = %q, want it to name embedder.dimensions and the model", msg)
	}
}

// Authorization must be absent, not blank: servers read "Bearer " as malformed, not anonymous.
func TestPluginBuildsWithoutAnAPIKey(t *testing.T) {
	ts := httpxtest.NewServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if got := r.Header.Values("Authorization"); len(got) != 0 {
			t.Errorf("Authorization = %q, want the header absent", got)
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			httpxtest.WriteJSON(w, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"local answer"}}]}`)
		case "/v1/embeddings":
			httpxtest.WriteJSON(w, http.StatusOK, `{"data":[{"index":0,"embedding":[0.25,0.5]}]}`)
		default:
			t.Errorf("path = %q, want a chat or embeddings route", r.URL.Path)
		}
	})
	with := `{"preset":"lmstudio","base_url":"` + ts.URL + `"}`

	chat := testConfig(lore.CapabilityComplete, "qwen3-8b", with)
	chat.Secrets = nil
	completer, err := Plugin().NewProvider(chat)
	if err != nil {
		t.Fatalf("NewProvider without an api key: %v", err)
	}
	if _, err := completer.(lore.Completer).Complete(context.Background(), testSystem, testUser); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	embed := testConfig(lore.CapabilityEmbed, "bge-m3", with)
	embed.Secrets = nil
	embed.Dimensions = 2
	embedder, err := Plugin().NewProvider(embed)
	if err != nil {
		t.Fatalf("NewProvider without an api key: %v", err)
	}
	if _, err := embedder.(lore.Embedder).Embed(context.Background(), []string{"local text"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
}

func TestCompleteAgainstAnOpenAICompatibleServer(t *testing.T) {
	cases := []struct {
		name string
		with func(baseURL string) string
		path string
	}{
		{
			name: "default chat path",
			with: func(baseURL string) string { return `{"base_url":"` + baseURL + `"}` },
			path: "/v1/chat/completions",
		},
		{
			name: "overridden chat path",
			with: func(baseURL string) string {
				return `{"base_url":"` + baseURL + `","chat_path":"/paas/v4/chat/completions"}`
			},
			path: "/paas/v4/chat/completions",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httpxtest.NewServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				if r.URL.Path != tc.path {
					t.Errorf("path = %q, want %q", r.URL.Path, tc.path)
				}
				if want := "Bearer " + fakeKey; r.Header.Get("Authorization") != want {
					t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), want)
				}

				var got struct {
					Model    string `json:"model"`
					Messages []struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if got.Model != "glm-4.6" {
					t.Errorf("model = %q, want glm-4.6", got.Model)
				}
				if len(got.Messages) != 2 ||
					got.Messages[0].Role != "system" || got.Messages[0].Content != testSystem ||
					got.Messages[1].Role != "user" || got.Messages[1].Content != testUser {
					t.Errorf("messages = %+v, want system then user", got.Messages)
				}

				httpxtest.WriteJSON(w, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"Because reads dominated."}}]}`)
			})

			provider, err := Plugin().NewProvider(testConfig(lore.CapabilityComplete, "glm-4.6", tc.with(ts.URL)))
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}

			text, err := provider.(lore.Completer).Complete(context.Background(), testSystem, testUser)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if want := "Because reads dominated."; text != want {
				t.Errorf("text = %q, want %q", text, want)
			}
		})
	}
}

func TestErrorsNameThePresetBehindTheInstance(t *testing.T) {
	ts := httpxtest.NewServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		httpxtest.WriteJSON(w, http.StatusUnauthorized, `{"error":{"code":"1002","message":"invalid api key: `+fakeKey+`"}}`)
	})

	provider, err := Plugin().NewProvider(testConfig(lore.CapabilityComplete, "glm-4.6",
		`{"preset":"zai","base_url":"`+ts.URL+`"}`))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.(lore.Completer).Complete(context.Background(), testSystem, testUser)
	if err == nil {
		t.Fatal("Complete succeeded, want error")
	}
	if !strings.Contains(err.Error(), "openai-compatible/zai") {
		t.Errorf("error %q does not name the preset behind the instance", err)
	}
	if strings.Contains(err.Error(), fakeKey) {
		t.Errorf("error %q leaks the api key", err)
	}
}
