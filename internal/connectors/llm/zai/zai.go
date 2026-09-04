// Package zai completes text with Z.AI's GLM chat API, which serves the same
// protocol as OpenAI chat completions.
package zai

import (
	"github.com/setthasit/Lore/internal/connectors/llm/openai"
	"github.com/setthasit/Lore/sdk/httpx"
)

// DefaultBaseURL reaches the global endpoint; the China deployment is selected
// by overriding it with https://open.bigmodel.cn/api.
const DefaultBaseURL = "https://api.z.ai/api"

const chatPath = "/paas/v4/chat/completions"

// New builds a client for model at baseURL; empty baseURL means DefaultBaseURL.
func New(apiKey, model, baseURL string, opts ...openai.Option) (*openai.Client, error) {
	return openai.NewCompatible("zai", apiKey, model, httpx.Endpoint(baseURL, DefaultBaseURL, chatPath), opts...)
}
