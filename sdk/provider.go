package lore

import (
	"context"
	"time"
)

// CompleteTimeout covers a whole non-streamed generation, not one round trip.
const CompleteTimeout = 120 * time.Second

type Embedder interface {
	// Vectors are positionally aligned with texts even when the provider answers
	// out of order; empty texts yield no vectors and make no request.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	Dimensions() int
}

type Completer interface {
	// Non-empty text on success; a provider that answered with nothing errors.
	Complete(ctx context.Context, system, user string) (string, error)
}
