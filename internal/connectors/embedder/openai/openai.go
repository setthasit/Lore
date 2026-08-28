// Package openai embeds text with the OpenAI embeddings API over net/http.
//
// Errors are returned raw with context wrapping; classifying them into
// internalerror kinds is the service layer's job. Nothing here reads the
// environment or configuration: the API key, model, dimension count and base
// URL arrive as explicit constructor arguments.
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

// DefaultBaseURL is the public API root, used when the caller passes no base
// URL. The base URL is an argument so Azure deployments, proxies and local
// OpenAI-compatible gateways work through this same client.
const DefaultBaseURL = "https://api.openai.com"

const (
	provider       = "openai"
	embeddingsPath = "/v1/embeddings"

	// maxInputsPerRequest splits caller batches. The API accepts more inputs per
	// call than this, but every vector comes back inline: 512 inputs at 1536
	// dimensions is already a multi-megabyte response, and a smaller request
	// keeps the blast radius of one failure to a fraction of a sync batch.
	maxInputsPerRequest = 512

	// maxAttempts bounds the tries spent on one request, initial attempt
	// included. A sync round retries on its own schedule; a connector does not
	// sit on a provider outage.
	maxAttempts = 4

	baseBackoff = 500 * time.Millisecond
	maxBackoff  = 8 * time.Second

	// maxRetryAfter caps a server-supplied delay: a directive longer than this
	// outlives the sync round waiting on it, so the wait is capped and the
	// attempt budget decides the outcome.
	maxRetryAfter = 60 * time.Second

	// requestTimeout is the default per-request ceiling, generous because a full
	// batch of long chunks is real work on the provider's side.
	requestTimeout = 60 * time.Second

	// errorBodyLimit and messageLimit bound how much of a failure response
	// becomes an error string: enough for the API's JSON error object, not
	// enough for a gateway's HTML page.
	errorBodyLimit = 2048
	messageLimit   = 256

	// drainLimit is how much of an unread body is consumed before closing, so a
	// keep-alive connection survives to the next batch.
	drainLimit = 4096
)

// Embedder calls the OpenAI embeddings API. It is safe for concurrent use.
type Embedder struct {
	apiKey   string
	model    string
	dims     int
	endpoint string
	identity string
	client   *http.Client

	// sleep performs the backoff wait. It is a field so tests can assert the
	// computed delays without spending them.
	sleep func(context.Context, time.Duration) error
}

// Option adjusts an Embedder at construction.
type Option func(*Embedder)

// WithHTTPClient supplies the client used for API calls, replacing the default
// one with its per-request timeout.
func WithHTTPClient(client *http.Client) Option {
	return func(e *Embedder) {
		if client != nil {
			e.client = client
		}
	}
}

// New builds an Embedder for model at baseURL (empty means DefaultBaseURL),
// authenticating with apiKey.
//
// dims is the vector width the model is expected to return. It is part of the
// identity stored alongside the index and every response is checked against it,
// so a provider that silently changes width fails loudly instead of poisoning
// the vector table.
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

// Identity reports the vector space as "openai/<model>/<dims>".
func (e *Embedder) Identity() string { return e.identity }

// Embed returns one vector per text, in the order the texts were given.
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

// embedBatch embeds one request's worth of texts, retrying transient failures
// until the attempt budget runs out.
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

// post makes one attempt at the embeddings call.
func (e *Embedder) post(ctx context.Context, body []byte, want int) ([][]float32, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build embeddings request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		// A cancelled context is the caller's decision, not a fault to retry;
		// anything else at transport level is worth another attempt.
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
		// A body cut short by a dropped connection arrives as a decode failure,
		// which is worth another attempt; a genuinely malformed body costs the
		// same attempts and then fails with this cause.
		return nil, &transientError{err: fmt.Errorf("openai: decode embeddings response: %w", err)}
	}
	return e.collect(payload.Data, want)
}

// collect maps vectors back onto input positions by the response's own index,
// checking the invariants the vector table depends on: one vector per input, at
// the configured width.
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
	// Counts match, indexes are unique and in range, so every position is filled.
	return vectors, nil
}

// scrub removes the API key from provider-supplied text. A rejected key is
// echoed back in 401 bodies, and error strings are exactly what reaches logs and
// user output.
func (e *Embedder) scrub(text string) string {
	return strings.ReplaceAll(text, e.apiKey, "[redacted]")
}

// backoff is the delay before the attempt after the given one: the server's
// directive when it supplied one, otherwise an exponential schedule with equal
// jitter — half the window fixed so attempts keep spreading out, half random so
// parallel batches do not resynchronise into the same burst.
func backoff(attempt int, serverDirective time.Duration) time.Duration {
	if serverDirective > 0 {
		return min(serverDirective, maxRetryAfter)
	}
	window := min(baseBackoff<<(attempt-1), maxBackoff)
	return window/2 + time.Duration(rand.Int64N(int64(window/2)+1))
}

// retryAfter reads the server's pacing directive, delta-seconds or HTTP-date per
// RFC 9110. Zero means absent or unusable, leaving the exponential schedule in
// charge.
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

// sleepContext waits for d, or until the context ends.
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

// closeBody consumes what was left unread, bounded, so the connection returns to
// the pool instead of being dropped mid-response.
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

// transientError marks a failure another attempt could survive, carrying the
// server's requested delay when it supplied one.
type transientError struct {
	after time.Duration
	err   error
}

func (t *transientError) Error() string { return t.err.Error() }
func (t *transientError) Unwrap() error { return t.err }

// request is the embeddings call body. A dimensions field is deliberately
// absent: the configured width is an invariant responses are checked against,
// not a truncation asked of the provider, which keeps models predating that
// parameter working.
type request struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

type response struct {
	Data []embeddingData `json:"data"`
}

// embeddingData is one vector plus the input position it answers. The API makes
// no promise about response order, so Index, not slice position, decides where a
// vector lands.
type embeddingData struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}
