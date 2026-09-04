// Package ollama embeds text with a local Ollama daemon's embeddings API over net/http.
package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/httpx"
)

var _ lore.Embedder = (*Embedder)(nil)

const DefaultBaseURL = "http://127.0.0.1:11434"

const (
	embedPath = "/api/embed"

	// The daemon loads the model on the first call, which can take tens of seconds.
	requestTimeout = 120 * time.Second
)

// Embedder is safe for concurrent use.
type Embedder struct {
	model    string
	dims     int
	endpoint string
	header   http.Header
	client   *http.Client

	sleep func(context.Context, time.Duration) error
}

type Option func(*Embedder)

func WithHTTPClient(client *http.Client) Option {
	return func(e *Embedder) {
		if client != nil {
			e.client = client
		}
	}
}

// New builds an Embedder for model at baseURL; empty baseURL means DefaultBaseURL.
// The daemon is unauthenticated, so there is no credential to pass.
func New(model, baseURL string, dims int, opts ...Option) (*Embedder, error) {
	if model == "" {
		return nil, errors.New("ollama: model is empty")
	}
	if dims <= 0 {
		return nil, fmt.Errorf("ollama: dimensions must be positive, got %d", dims)
	}

	e := &Embedder{
		model:    model,
		dims:     dims,
		endpoint: httpx.Endpoint(baseURL, DefaultBaseURL, embedPath),
		header:   http.Header{"Content-Type": {"application/json"}},
		client:   &http.Client{Timeout: requestTimeout},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

// Dimensions is the width every vector this Embedder returns carries; the host
// composes the vector-space identity from it.
func (e *Embedder) Dimensions() int { return e.dims }

func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	for i, text := range texts {
		if text == "" {
			return nil, fmt.Errorf("ollama: texts[%d] is empty", i)
		}
	}

	body, err := json.Marshal(request{Model: e.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("ollama: encode embeddings request: %w", err)
	}

	call := httpx.Client{HTTP: e.client, Sleep: e.sleep, Op: "ollama: embeddings"}
	var payload response
	if err := call.PostJSON(ctx, e.endpoint, e.header, body, &payload); err != nil {
		return nil, err
	}
	if payload.Error != "" {
		return nil, fmt.Errorf("ollama: embeddings: %s", payload.Error)
	}
	return e.checked(payload.Embeddings, len(texts))
}

func (e *Embedder) checked(vectors [][]float32, want int) ([][]float32, error) {
	if len(vectors) != want {
		return nil, fmt.Errorf("ollama: embeddings: got %d vectors for %d inputs", len(vectors), want)
	}
	for i, vector := range vectors {
		if len(vector) != e.dims {
			return nil, fmt.Errorf("ollama: model %q returned %d dimensions for input %d, want %d", e.model, len(vector), i, e.dims)
		}
	}
	return vectors, nil
}

type request struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// The daemon answers in input order and offers no index to reorder by. It also
// states a refusal in Error under a 200, not only under a failing status.
type response struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}
