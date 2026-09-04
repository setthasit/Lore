// Package openai completes text with the OpenAI chat completions API and embeds text with the OpenAI embeddings API, both over net/http.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/httpx"
)

var _ lore.Completer = (*Client)(nil)

const DefaultBaseURL = "https://api.openai.com"

const (
	chatPath = "/v1/chat/completions"

	// providerName prefixes this driver's own errors. A caller reaching another
	// vendor over the same protocol passes its own name instead, so an error
	// never blames OpenAI for a gateway's answer.
	providerName = "openai"
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
	return NewCompatible(providerName, apiKey, model, httpx.Endpoint(baseURL, DefaultBaseURL, chatPath), opts...)
}

// NewCompatible builds a client for another provider serving this same protocol
// at endpoint, naming it provider in errors.
func NewCompatible(provider, apiKey, model, endpoint string, opts ...Option) (*Client, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("%s: api key is empty", provider)
	}
	if model == "" {
		return nil, fmt.Errorf("%s: model is empty", provider)
	}

	header := http.Header{}
	header.Set("content-type", "application/json")
	header.Set("authorization", "Bearer "+apiKey)

	c := &Client{
		model:    model,
		endpoint: endpoint,
		header:   header,
		call: httpx.Client{
			HTTP:   &http.Client{Timeout: lore.CompleteTimeout},
			Op:     provider + ": chat completions",
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
		return "", errors.New(c.call.Op + ": user prompt is empty")
	}

	messages := make([]message, 0, 2)
	if system != "" {
		messages = append(messages, message{Role: "system", Content: system})
	}
	messages = append(messages, message{Role: "user", Content: user})

	body, err := json.Marshal(request{Model: c.model, Messages: messages})
	if err != nil {
		return "", fmt.Errorf("%s: encode request: %w", c.call.Op, err)
	}

	var payload response
	if err := c.call.PostJSON(ctx, c.endpoint, c.header, body, &payload); err != nil {
		return "", err
	}
	if len(payload.Choices) == 0 || payload.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("%s: model %q answered with no text", c.call.Op, c.model)
	}
	return payload.Choices[0].Message.Content, nil
}

type request struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type response struct {
	Choices []choice `json:"choices"`
}

type choice struct {
	Message message `json:"message"`
}
