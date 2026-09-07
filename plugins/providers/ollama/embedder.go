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

const (
	embedPath = "/api/embed"

	// The daemon loads the model on the first call, which can take tens of seconds.
	embedTimeout = 120 * time.Second
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

// NewEmbedder builds an Embedder for model at baseURL; empty baseURL means DefaultBaseURL.
func NewEmbedder(model, baseURL string, dims int) (*Embedder, error) {
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
		client:   &http.Client{Timeout: embedTimeout},
	}
	return e, nil
}

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

	body, err := json.Marshal(embedRequest{Model: e.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("ollama: encode embeddings request: %w", err)
	}

	call := httpx.Client{HTTP: e.client, Sleep: e.sleep, Op: "ollama: embeddings"}
	var payload embedResponse
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

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// The daemon answers in input order and offers no index to reorder by. It also
// states a refusal in Error under a 200, not only under a failing status.
type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}
