// Package anthropic completes text with the Anthropic messages API over net/http.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/httpx"
)

var _ lore.Completer = (*Client)(nil)

const DefaultBaseURL = "https://api.anthropic.com"

const (
	messagesPath = "/v1/messages"
	apiVersion   = "2023-06-01"

	// The API rejects a request without max_tokens, so it is a ceiling set far
	// above the longest synthesis answer rather than a tuning knob.
	maxTokens = 4096
)

// Client is safe for concurrent use.
type Client struct {
	model    string
	endpoint string
	header   http.Header
	call     httpx.Client
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
func New(apiKey, model, baseURL string, opts ...Option) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("anthropic: api key is empty")
	}
	if model == "" {
		return nil, errors.New("anthropic: model is empty")
	}

	header := http.Header{}
	header.Set("content-type", "application/json")
	header.Set("x-api-key", apiKey)
	header.Set("anthropic-version", apiVersion)

	c := &Client{
		model:    model,
		endpoint: httpx.Endpoint(baseURL, DefaultBaseURL, messagesPath),
		header:   header,
		call: httpx.Client{
			HTTP:   &http.Client{Timeout: lore.CompleteTimeout},
			Op:     "anthropic: messages",
			Secret: apiKey,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	if user == "" {
		return "", errors.New("anthropic: messages: user prompt is empty")
	}

	body, err := json.Marshal(request{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  []message{{Role: "user", Content: user}},
	})
	if err != nil {
		return "", fmt.Errorf("anthropic: messages: encode request: %w", err)
	}

	var payload response
	if err := c.call.PostJSON(ctx, c.endpoint, c.header, body, &payload); err != nil {
		return "", err
	}

	answer := joinText(payload.Content)
	if answer == "" {
		return "", fmt.Errorf("anthropic: messages: model %q answered with no text block", c.model)
	}
	return answer, nil
}

// A reply may lead with a non-text block, so position does not identify the answer.
func joinText(blocks []block) string {
	var answer strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			answer.WriteString(b.Text)
		}
	}
	return answer.String()
}

// The system prompt is a top-level field here, never a message role.
type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type response struct {
	Content []block `json:"content"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
