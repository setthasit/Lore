// Package httpretry posts JSON to an HTTP API and retries the failures a
// server reports as temporary: 429, 5xx, a transport error, a truncated body.
package httpretry

import (
	"bytes"
	"cmp"
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
)

const (
	// MaxAttempts bounds the tries spent on one request, initial attempt included.
	MaxAttempts = 4

	BaseBackoff = 500 * time.Millisecond
)

const (
	maxBackoff = 8 * time.Second

	// maxRetryAfter caps a server-supplied delay; the attempt budget then decides.
	maxRetryAfter = 60 * time.Second

	errorBodyLimit = 2048
	messageLimit   = 256

	drainLimit = 4096

	redaction = "[redacted]"
)

func Endpoint(baseURL, defaultBaseURL, path string) string {
	return strings.TrimSuffix(cmp.Or(baseURL, defaultBaseURL), "/") + path
}

type Client struct {
	HTTP *http.Client

	// Sleep waits out a retry delay; nil waits on the real clock.
	Sleep func(context.Context, time.Duration) error

	// Op prefixes every error, as in "openai: embeddings".
	Op string

	// Secret is replaced by "[redacted]" wherever a failure body echoes it back.
	Secret string
}

func (c Client) PostJSON(ctx context.Context, url string, header http.Header, body []byte, out any) error {
	for attempt := 1; ; attempt++ {
		err := c.post(ctx, url, header, body, out)
		if err == nil {
			return nil
		}

		var transient *transientError
		if !errors.As(err, &transient) {
			return err
		}
		if attempt == MaxAttempts {
			return fmt.Errorf("%s: giving up after %d attempts: %w", c.Op, MaxAttempts, err)
		}
		if waitErr := c.wait(ctx, backoff(attempt, transient.after)); waitErr != nil {
			return fmt.Errorf("%s: waiting to retry: %w", c.Op, waitErr)
		}
	}
}

func (c Client) post(ctx context.Context, url string, header http.Header, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", c.Op, err)
	}
	for name, values := range header {
		req.Header[name] = values
	}

	resp, err := cmp.Or(c.HTTP, http.DefaultClient).Do(req)
	if err != nil {
		// A cancelled context is the caller's decision, not a fault to retry.
		if ctx.Err() != nil {
			return fmt.Errorf("%s: request: %w", c.Op, err)
		}
		return &transientError{err: fmt.Errorf("%s: request: %w", c.Op, err)}
	}
	defer closeBody(resp.Body)

	if resp.StatusCode/100 != 2 {
		statusErr := fmt.Errorf("%s: %s: %s", c.Op, resp.Status, c.errorMessage(resp.Body))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
			return &transientError{after: retryAfter(resp.Header), err: statusErr}
		}
		return statusErr
	}

	// A body cut short by a dropped connection arrives as a decode failure, so
	// the retry decision is made on raw bytes before out is written once.
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return &transientError{err: fmt.Errorf("%s: decode response: %w", c.Op, err)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", c.Op, err)
	}
	return nil
}

func (c Client) wait(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// errorMessage extracts the API's own explanation of a failure, scrubbing the
// secret before truncation so a boundary cut cannot leave a key prefix behind.
func (c Client) errorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, errorBodyLimit))
	if err != nil || len(raw) == 0 {
		return "no error body"
	}

	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &payload) == nil {
		if text := apiError(payload.Error); text != "" {
			return c.readable(text)
		}
	}
	return c.readable(strings.TrimSpace(string(raw)))
}

func (c Client) readable(text string) string {
	if c.Secret != "" {
		text = strings.ReplaceAll(text, c.Secret, redaction)
	}
	if len(text) <= messageLimit {
		return text
	}
	return strings.ToValidUTF8(text[:messageLimit], "") + "…"
}

// Providers state a failure as {"error":"…"} or as an object whose machine-readable
// kind is called "code" by OpenAI and "type" by Anthropic.
func apiError(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}

	var payload struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Message == "" {
		return ""
	}
	if kind := cmp.Or(payload.Code, payload.Type); kind != "" {
		return kind + ": " + payload.Message
	}
	return payload.Message
}

// Equal jitter: half the window fixed, half random so parallel callers do not resync.
func backoff(attempt int, serverDirective time.Duration) time.Duration {
	if serverDirective > 0 {
		return min(serverDirective, maxRetryAfter)
	}
	window := min(BaseBackoff<<(attempt-1), maxBackoff)
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

// Draining the unread body lets the connection return to the pool.
func closeBody(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainLimit))
	_ = body.Close()
}

type transientError struct {
	after time.Duration
	err   error
}

func (t *transientError) Error() string { return t.err.Error() }
func (t *transientError) Unwrap() error { return t.err }
