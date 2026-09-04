package lore

import (
	"context"
	"time"
)

// CompleteTimeout covers a whole non-streamed generation, not one round trip.
const CompleteTimeout = 120 * time.Second

// Embedder is the embedding capability of a provider plugin.
type Embedder interface {
	// Embed returns one vector per text, positionally aligned with texts even
	// when the provider answers out of order. Empty texts: no vectors, no request.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions is the vector width every returned vector carries. The host
	// composes the vector-space identity from the plugin name, the configured
	// model and this width, so a plugin cannot claim another's identity.
	Dimensions() int
}

// Completer is the text-completion capability of a provider plugin.
type Completer interface {
	// Complete answers user under the system prompt. Success carries non-empty
	// text: a provider that answered with nothing returns an error instead.
	Complete(ctx context.Context, system, user string) (string, error)
}
