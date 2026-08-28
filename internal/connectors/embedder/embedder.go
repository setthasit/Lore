package embedder

import (
	"context"
	"fmt"
)

// Embedder turns text into the vectors that back semantic retrieval.
//
// Implementations are connectors: they return raw errors with context wrapping
// and leave classification to the service layer.
type Embedder interface {
	// Embed returns one vector per text, positionally aligned with texts, all
	// of the width encoded in Identity. Providers that answer out of order
	// reorder before returning, so callers can zip the results back onto their
	// chunks by index. An empty texts slice yields no vectors and no request.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Identity names the vector space the returned vectors live in, as
	// "provider/model/dims" — for example "openai/text-embedding-3-small/1536".
	// It is stored in the index's meta table and compared at startup: vectors
	// are only comparable within one space, so a change to any component
	// (provider, model, or dimension count) invalidates every stored vector and
	// forces a re-embed rather than silently degrading recall. The value is
	// stable for the lifetime of an Embedder.
	Identity() string
}

// FormatIdentity builds the canonical Identity string, so every provider spells
// the vector space the same way and stored identities stay comparable across
// implementations.
func FormatIdentity(provider, model string, dims int) string {
	return fmt.Sprintf("%s/%s/%d", provider, model, dims)
}
