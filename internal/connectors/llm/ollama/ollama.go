// Package ollama completes text with a local Ollama daemon's chat API over net/http.
package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"lore/internal/connectors/httpretry"
	"lore/internal/connectors/llm"
)

var _ llm.LLM = (*Client)(nil)

const DefaultBaseURL = "http://127.0.0.1:11434"

const chatPath = "/api/chat"

// Client is safe for concurrent use.
type Client struct {
	model    string
	endpoint string
	header   http.Header
	call     httpretry.Client
}

type Option func(*Client)

func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.call.HTTP = client
		}
	}
}

// New builds a client for model at baseURL; empty baseURL means DefaultBaseURL.
// The daemon is unauthenticated, so there is no credential to pass.
func New(model, baseURL string, opts ...Option) (*Client, error) {
	if model == "" {
		return nil, errors.New("ollama: model is empty")
	}

	c := &Client{
		model:    model,
		endpoint: httpretry.Endpoint(baseURL, DefaultBaseURL, chatPath),
		header:   http.Header{"Content-Type": {"application/json"}},
		call: httpretry.Client{
			HTTP: &http.Client{Timeout: llm.RequestTimeout},
			Op:   "ollama: chat",
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	if user == "" {
		return "", errors.New("ollama: chat: user prompt is empty")
	}

	messages := make([]message, 0, 2)
	if system != "" {
		messages = append(messages, message{Role: "system", Content: system})
	}
	messages = append(messages, message{Role: "user", Content: user})

	body, err := json.Marshal(request{Model: c.model, Messages: messages})
	if err != nil {
		return "", fmt.Errorf("ollama: chat: encode request: %w", err)
	}

	var payload response
	if err := c.call.PostJSON(ctx, c.endpoint, c.header, body, &payload); err != nil {
		return "", err
	}
	if payload.Error != "" {
		return "", fmt.Errorf("ollama: chat: %s", payload.Error)
	}
	if payload.Message.Content == "" {
		return "", fmt.Errorf("ollama: chat: model %q answered with no text", c.model)
	}
	return payload.Message.Content, nil
}

// The daemon answers with an NDJSON stream unless stream is false.
type request struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// The daemon states a refusal in Error under a 200, not only under a failing status.
type response struct {
	Message message `json:"message"`
	Error   string  `json:"error"`
}
