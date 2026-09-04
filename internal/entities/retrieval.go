package entities

import (
	"time"

	"github.com/setthasit/Lore/sdk"
)

// Zero-valued fields do not constrain.
type Filters struct {
	Source      string
	RepoRef     string
	DocType     lore.DocType
	CreatedFrom time.Time
	CreatedTo   time.Time
}

type Chunk struct {
	DocID     lore.DocID
	Ordinal   int
	Text      string
	Source    string
	RepoRef   string
	DocType   lore.DocType
	Author    string
	CreatedAt time.Time
	UpdatedAt time.Time
	ThreadID  string

	// Nil means not embedded: the chunk is indexed lexically only.
	Embedding []float32
}

// Score is higher-is-better; the store negates BM25 and vector distance. Scores
// are comparable within one result list only, never across search strategies.
type ChunkHit struct {
	Chunk
	Score float32
}
