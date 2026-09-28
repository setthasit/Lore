// Package httpxtest scripts per-attempt responses for httpx.Client tests.
package httpxtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

type Server struct {
	*httptest.Server

	mu    sync.Mutex
	calls int
}

// NewServer calls handler with the 1-based attempt number.
func NewServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, attempt int)) *Server {
	t.Helper()

	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls++
		attempt := s.calls
		s.mu.Unlock()

		handler(w, r, attempt)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func WriteJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// WaitRecorder stands in for the backoff sleep, recording delays without spending them.
type WaitRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (r *WaitRecorder) Sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waits = append(r.waits, d)
	return ctx.Err()
}

func (r *WaitRecorder) Recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.waits)
}
