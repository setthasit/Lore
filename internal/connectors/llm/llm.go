// Package llm defines the text completion provider interface used for synthesis.
package llm

import (
	"context"
	"time"
)

// RequestTimeout covers a whole non-streamed generation, not one round trip.
const RequestTimeout = 120 * time.Second

type LLM interface {
	// Complete answers user under the system prompt. Success carries non-empty
	// text: a provider that answered with nothing returns an error instead.
	Complete(ctx context.Context, system, user string) (string, error)
}
