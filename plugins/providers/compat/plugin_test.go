package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/httpx/httpxtest"
)

const (
	// An obviously fake credential: these tests must never need a real one.
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

// Every capability the manifest declares must build a value satisfying the SDK
// interface the host will assert it against, or the manifest is a lie.
func TestPluginBuildsEveryDeclaredCapability(t *testing.T) {
	tests := []struct {
		capability lore.Capability
		check      func(t *testing.T, provider lore.Provider)
	}{
		{
			capability: lore.CapabilityEmbed,
			check: func(t *testing.T, provider lore.Provider) {
				embedder, ok := provider.(lore.Embedder)
				if !ok {
					t.Fatalf("provider %T does not implement lore.Embedder", provider)
				}
				if got := embedder.Dimensions(); got != 1024 {
					t.Errorf("Dimensions() = %d, want 1024", got)
				}
			},
		},
		{
			capability: lore.CapabilityComplete,
			check: func(t *testing.T, provider lore.Provider) {
				if _, ok := provider.(lore.Completer); !ok {
					t.Fatalf("provider %T does not implement lore.Completer", provider)
				}
			},
		},
	}

	declared := Plugin().Manifest().Capabilities.Names()
	for _, tt := range tests {
		if !slices.Contains(declared, tt.capability) {
			t.Errorf("capability %s is tested but not declared", tt.capability)
		}

		t.Run(string(tt.capability), func(t *testing.T) {
			provider, err := Plugin().NewProvider(testConfig(tt.capability, "some-model", `{"preset":"together"}`))
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			tt.check(t, provider)
		})
	}

	if len(tests) != len(declared) {
		t.Errorf("declared capabilities %v, but %d are covered", declared, len(tests))
	}
}

func TestPluginRefusesUndeclaredCapability(t *testing.T) {
	_, err := Plugin().NewProvider(testConfig("rerank", "some-model", `{"preset":"groq"}`))
	if err == nil {
		t.Fatal("NewProvider succeeded for a capability the manifest does not declare")
	}

	const want = "openai-compatible: capability rerank is not served by this provider; it serves embed, complete"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// Neither key set is the one configuration this driver cannot guess its way
// out of, so the error has to offer both ways out.
func TestPluginRefusesWithoutPresetOrBaseURL(t *testing.T) {
	_, err := Plugin().NewProvider(testConfig(lore.CapabilityComplete, "some-model", `{}`))
	if err == nil {
		t.Fatal("NewProvider succeeded with neither preset nor base_url")
	}

	const want = "openai-compatible: set preset to one of deepseek, groq, lmstudio, moonshot, openrouter, together, vllm, zai, " +
		"or set base_url to the vendor's OpenAI-compatible base URL"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestPluginRefusesUnknownPreset(t *testing.T) {
	_, err := Plugin().NewProvider(testConfig(lore.CapabilityComplete, "some-model", `{"preset":"mistral"}`))
	if err == nil {
		t.Fatal("NewProvider succeeded for a preset the table does not hold")
	}

	const want = "openai-compatible: preset mistral is not one this build knows; " +
		"known presets: deepseek, groq, lmstudio, moonshot, openrouter, together, vllm, zai"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
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

// A binding that names no model gets the row's default, which is the only use
// the driver makes of it: the manifest never suggests one for nine vendors.
func TestPresetDefaultModelAnswersAnUnnamedModel(t *testing.T) {
	ts := httpxtest.NewServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var got struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if want := "moonshotai/kimi-k2"; got.Model != want {
			t.Errorf("model = %q, want the openrouter row's default %q", got.Model, want)
		}
		httpxtest.WriteJSON(w, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	})

	provider, err := Plugin().NewProvider(testConfig(lore.CapabilityComplete, "",
		`{"preset":"openrouter","base_url":"`+ts.URL+`"}`))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := provider.(lore.Completer).Complete(context.Background(), testSystem, testUser); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// A self-hosted row carries no default, because the model is whichever one the
// operator loaded; the error says so rather than inventing a name.
func TestPluginRefusesUnnamedModelWithoutADefault(t *testing.T) {
	cases := []struct {
		name string
		with string
		want string
	}{
		{
			name: "self-hosted preset",
			with: `{"preset":"vllm"}`,
			want: "openai-compatible: name the complete model in the role binding: preset vllm carries no default, because the model is whichever one you serve",
		},
		{
			name: "no preset at all",
			with: `{"base_url":"https://llm.internal"}`,
			want: "openai-compatible: name the complete model in the role binding: no preset was chosen, so there is no default to fall back on",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Plugin().NewProvider(testConfig(lore.CapabilityComplete, "", tc.with))
			if err == nil {
				t.Fatal("NewProvider succeeded with no model to build on")
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err, tc.want)
			}
		})
	}
}

// The width follows the model, and this driver knows nobody's models, so the
// operator declares it: a wrong width poisons every vector written under it.
func TestPluginRequiresDeclaredDimensions(t *testing.T) {
	cfg := testConfig(lore.CapabilityEmbed, "bge-m3", `{"preset":"vllm"}`)
	cfg.Dimensions = 0

	_, err := Plugin().NewProvider(cfg)
	if err == nil {
		t.Fatal("NewProvider built an embedder of unknown width")
	}

	const want = "openai-compatible: embedder.dimensions must be set to the vector width of bge-m3: this driver serves any vendor, so its models imply no width"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// A local server authenticates nothing, so an unset key must still build.
func TestPluginBuildsWithoutAnAPIKey(t *testing.T) {
	cfg := testConfig(lore.CapabilityComplete, "qwen3-8b", `{"preset":"lmstudio"}`)
	cfg.Secrets = nil

	if _, err := Plugin().NewProvider(cfg); err != nil {
		t.Fatalf("NewProvider without an api key: %v", err)
	}
}

func TestPluginRejectsUnknownConfigKey(t *testing.T) {
	_, err := Plugin().NewProvider(testConfig(lore.CapabilityComplete, "glm-4.6", `{"presset":"zai"}`))
	if err == nil {
		t.Fatal("NewProvider accepted a key the manifest does not declare")
	}
	if !strings.Contains(err.Error(), "presset") {
		t.Errorf("error = %q, want it to name the unknown key", err)
	}
}

// End to end over the real protocol: configuration alone reaches a vendor, on
// the driver's default path and on an overridden one.
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

// The preset reaches the errors an operator sees, because a workspace may hold
// several instances of this one plugin.
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
