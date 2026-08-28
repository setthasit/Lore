// Package openai embeds text with the OpenAI embeddings API over net/http.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lore/internal/connectors/embedder"
)

var _ embedder.Embedder = (*Embedder)(nil)

const DefaultBaseURL = "https://api.openai.com"

const (
	provider       = "openai"
	embeddingsPath = "/v1/embeddings"

	// maxInputsPerRequest splits caller batches: every vector comes back inline,
	// and 512 inputs at 1536 dimensions is already a multi-megabyte response.
	maxInputsPerRequest = 512

	// maxAttempts bounds the tries spent on one request, initial attempt included.
	maxAttempts = 4

	baseBackoff = 500 * time.Millisecond
	maxBackoff  = 8 * time.Second

	// maxRetryAfter caps a server-supplied delay; the attempt budget then decides.
	maxRetryAfter = 60 * time.Second

	requestTimeout = 60 * time.Second

	// errorBodyLimit and messageLimit bound how much of a failure body reaches an error.
	errorBodyLimit = 2048
	messageLimit   = 256

	drainLimit = 4096
)

// Embedder is safe for concurrent use.
type Embedder struct {
	apiKey   string
	model    string
	dims     int
	endpoint string
	identity string
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
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	e := &Embedder{
		apiKey:   apiKey,
		model:    model,
		dims:     dims,
		endpoint: strings.TrimSuffix(baseURL, "/") + embeddingsPath,
		identity: embedder.FormatIdentity(provider, model, dims),
		client:   &http.Client{Timeout: requestTimeout},
		sleep:    sleepContext,
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

	for attempt := 1; ; attempt++ {
		vectors, err := e.post(ctx, body, len(texts))
		if err == nil {
			return vectors, nil
		}

		var transient *transientError
		if !errors.As(err, &transient) {
			return nil, err
		}
		if attempt == maxAttempts {
			return nil, fmt.Errorf("openai: embeddings failed after %d attempts: %w", maxAttempts, err)
		}
		if waitErr := e.sleep(ctx, backoff(attempt, transient.after)); waitErr != nil {
			return nil, fmt.Errorf("openai: waiting to retry embeddings: %w", waitErr)
		}
	}
}

func (e *Embedder) post(ctx context.Context, body []byte, want int) ([][]float32, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build embeddings request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		// A cancelled context is the caller's decision, not a fault to retry.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("openai: embeddings request: %w", err)
		}
		return nil, &transientError{err: fmt.Errorf("openai: embeddings request: %w", err)}
	}
	defer closeBody(resp.Body)

	if resp.StatusCode != http.StatusOK {
		statusErr := fmt.Errorf("openai: embeddings: %s: %s", resp.Status, e.errorMessage(resp.Body))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
			return nil, &transientError{after: retryAfter(resp.Header), err: statusErr}
		}
		return nil, statusErr
	}

	var payload response
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		// A body cut short by a dropped connection arrives as a decode failure.
		return nil, &transientError{err: fmt.Errorf("openai: decode embeddings response: %w", err)}
	}
	return e.collect(payload.Data, want)
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

// A rejected key is echoed back in 401 bodies, and error strings reach logs.
func (e *Embedder) scrub(text string) string {
	return strings.ReplaceAll(text, e.apiKey, "[redacted]")
}

// Equal jitter: half the window fixed, half random so parallel batches do not resync.
func backoff(attempt int, serverDirective time.Duration) time.Duration {
	if serverDirective > 0 {
		return min(serverDirective, maxRetryAfter)
	}
	window := min(baseBackoff<<(attempt-1), maxBackoff)
	return window/2 + time.Duration(rand.Int64N(int64(window/2)+1))
}

// retryAfter reads delta-seconds or an HTTP-date per RFC 9110; zero means unusable.
func retryAfter(header http.Header) time.Duration {
	value := header.Get("Retry-After")
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Draining the unread body lets the connection return to the pool.
func closeBody(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainLimit))
	_ = body.Close()
}

// errorMessage extracts the API's own explanation of a failure, scrubbing the
// key before truncation so a boundary cut can never leave a key prefix behind.
func (e *Embedder) errorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, errorBodyLimit))
	if err != nil || len(raw) == 0 {
		return "no error body"
	}

	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Error.Message != "" {
		if payload.Error.Code != "" {
			return payload.Error.Code + ": " + truncate(e.scrub(payload.Error.Message))
		}
		return truncate(e.scrub(payload.Error.Message))
	}
	return truncate(e.scrub(strings.TrimSpace(string(raw))))
}

func truncate(text string) string {
	if len(text) <= messageLimit {
		return text
	}
	return strings.ToValidUTF8(text[:messageLimit], "") + "…"
}

type transientError struct {
	after time.Duration
	err   error
}

func (t *transientError) Error() string { return t.err.Error() }
func (t *transientError) Unwrap() error { return t.err }

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
