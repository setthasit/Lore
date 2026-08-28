package embedder

import (
	"context"
	"fmt"
)

type Embedder interface {
	// Embed returns one vector per text, positionally aligned with texts even
	// when the provider answers out of order. Empty texts: no vectors, no request.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Identity names the vector space as "provider/model/dims". A change to any
	// component invalidates every stored vector and forces a re-embed.
	Identity() string
}

func FormatIdentity(provider, model string, dims int) string {
	return fmt.Sprintf("%s/%s/%d", provider, model, dims)
}
