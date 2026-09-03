// Package openai embeds text with the OpenAI embeddings API over net/http.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/setthasit/Lore/internal/connectors/embedder"
	"github.com/setthasit/Lore/internal/connectors/httpretry"
)

var _ embedder.Embedder = (*Embedder)(nil)

const DefaultBaseURL = "https://api.openai.com"

const (
	provider       = "openai"
	embeddingsPath = "/v1/embeddings"

	// maxInputsPerRequest splits caller batches: every vector comes back inline,
	// and 512 inputs at 1536 dimensions is already a multi-megabyte response.
	maxInputsPerRequest = 512

	requestTimeout = 60 * time.Second
)

// Embedder is safe for concurrent use.
type Embedder struct {
	apiKey   string
	model    string
	dims     int
	endpoint string
	identity string
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
func New(apiKey, model, baseURL string, dims int, opts ...Option) (*Embedder, error) {
	if apiKey == "" {
		return nil, errors.New("openai: api key is empty")
	}
	if model == "" {
		return nil, errors.New("openai: model is empty")
	}
	if dims <= 0 {
		return nil, fmt.Errorf("openai: dimensions must be positive, got %d", dims)
	}

	e := &Embedder{
		apiKey:   apiKey,
		model:    model,
		dims:     dims,
		endpoint: httpretry.Endpoint(baseURL, DefaultBaseURL, embeddingsPath),
		identity: embedder.FormatIdentity(provider, model, dims),
		header: http.Header{
			"Content-Type":  {"application/json"},
			"Authorization": {"Bearer " + apiKey},
		},
		client: &http.Client{Timeout: requestTimeout},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

func (e *Embedder) Identity() string { return e.identity }

func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	for i, text := range texts {
		if text == "" {
			return nil, fmt.Errorf("openai: texts[%d] is empty", i)
		}
	}

	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxInputsPerRequest {
		batch, err := e.embedBatch(ctx, texts[start:min(start+maxInputsPerRequest, len(texts))])
		if err != nil {
			return nil, err
		}
		vectors = append(vectors, batch...)
	}
	return vectors, nil
}

func (e *Embedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(request{Input: texts, Model: e.model})
	if err != nil {
		return nil, fmt.Errorf("openai: encode embeddings request: %w", err)
	}

	call := httpretry.Client{HTTP: e.client, Sleep: e.sleep, Op: "openai: embeddings", Secret: e.apiKey}
	var payload response
	if err := call.PostJSON(ctx, e.endpoint, e.header, body, &payload); err != nil {
		return nil, err
	}
	return e.collect(payload.Data, len(texts))
}

func (e *Embedder) collect(data []embeddingData, want int) ([][]float32, error) {
	if len(data) != want {
		return nil, fmt.Errorf("openai: embeddings: got %d vectors for %d inputs", len(data), want)
	}

	vectors := make([][]float32, want)
	for _, item := range data {
		if item.Index < 0 || item.Index >= want {
			return nil, fmt.Errorf("openai: embeddings: response index %d outside 0..%d", item.Index, want-1)
		}
		if vectors[item.Index] != nil {
			return nil, fmt.Errorf("openai: embeddings: duplicate response index %d", item.Index)
		}
		if len(item.Embedding) != e.dims {
			return nil, fmt.Errorf("openai: model %q returned %d dimensions, want %d", e.model, len(item.Embedding), e.dims)
		}
		vectors[item.Index] = item.Embedding
	}
	return vectors, nil
}

// No dimensions field: the width is checked, not requested, so older models still work.
type request struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

type response struct {
	Data []embeddingData `json:"data"`
}

// The API makes no promise about response order, so Index decides where a vector lands.
type embeddingData struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}
