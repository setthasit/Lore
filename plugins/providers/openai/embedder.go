package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/httpx"
)

var _ lore.Embedder = (*Embedder)(nil)

const (
	embeddingsPath = "/v1/embeddings"

	// maxInputsPerRequest splits caller batches: every vector comes back inline,
	// and 512 inputs at 1536 dimensions is already a multi-megabyte response.
	maxInputsPerRequest = 512

	embedTimeout = 60 * time.Second
)

// Embedder is safe for concurrent use.
type Embedder struct {
	provider string
	apiKey   string
	model    string
	dims     int
	endpoint string
	header   http.Header
	client   *http.Client

	sleep func(context.Context, time.Duration) error
}

type EmbedderOption func(*Embedder)

func WithEmbedderHTTPClient(client *http.Client) EmbedderOption {
	return func(e *Embedder) {
		if client != nil {
			e.client = client
		}
	}
}

// NewEmbedder builds an Embedder for model at baseURL; empty baseURL means DefaultBaseURL.
func NewEmbedder(apiKey, model, baseURL string, dims int, opts ...EmbedderOption) (*Embedder, error) {
	return NewEmbedderAt(providerName, apiKey, model, httpx.Endpoint(baseURL, DefaultBaseURL, embeddingsPath), dims, opts...)
}

// NewEmbedderAt builds an Embedder for another provider serving this same
// protocol at endpoint, naming it provider in errors. It is the embeddings
// counterpart of NewCompatible: a gateway may put the embeddings route
// anywhere, so the whole endpoint is the caller's to decide.
func NewEmbedderAt(provider, apiKey, model, endpoint string, dims int, opts ...EmbedderOption) (*Embedder, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("%s: api key is empty", provider)
	}
	if model == "" {
		return nil, fmt.Errorf("%s: model is empty", provider)
	}
	if dims <= 0 {
		return nil, fmt.Errorf("%s: dimensions must be positive, got %d", provider, dims)
	}

	e := &Embedder{
		provider: provider,
		apiKey:   apiKey,
		model:    model,
		dims:     dims,
		endpoint: endpoint,
		header: http.Header{
			"Content-Type":  {"application/json"},
			"Authorization": {"Bearer " + apiKey},
		},
		client: &http.Client{Timeout: embedTimeout},
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
			return nil, fmt.Errorf("%s: texts[%d] is empty", e.provider, i)
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
	body, err := json.Marshal(embedRequest{Input: texts, Model: e.model})
	if err != nil {
		return nil, fmt.Errorf("%s: encode embeddings request: %w", e.provider, err)
	}

	call := httpx.Client{HTTP: e.client, Sleep: e.sleep, Op: e.provider + ": embeddings", Secret: e.apiKey}
	var payload embedResponse
	if err := call.PostJSON(ctx, e.endpoint, e.header, body, &payload); err != nil {
		return nil, err
	}
	return e.collect(payload.Data, len(texts))
}

func (e *Embedder) collect(data []embeddingData, want int) ([][]float32, error) {
	if len(data) != want {
		return nil, fmt.Errorf("%s: embeddings: got %d vectors for %d inputs", e.provider, len(data), want)
	}

	vectors := make([][]float32, want)
	for _, item := range data {
		if item.Index < 0 || item.Index >= want {
			return nil, fmt.Errorf("%s: embeddings: response index %d outside 0..%d", e.provider, item.Index, want-1)
		}
		if vectors[item.Index] != nil {
			return nil, fmt.Errorf("%s: embeddings: duplicate response index %d", e.provider, item.Index)
		}
		if len(item.Embedding) != e.dims {
			return nil, fmt.Errorf("%s: model %q returned %d dimensions, want %d", e.provider, e.model, len(item.Embedding), e.dims)
		}
		vectors[item.Index] = item.Embedding
	}
	return vectors, nil
}

// No dimensions field: the width is checked, not requested, so older models still work.
type embedRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

type embedResponse struct {
	Data []embeddingData `json:"data"`
}

// The API makes no promise about response order, so Index decides where a vector lands.
type embeddingData struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}
