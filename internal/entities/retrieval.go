package entities

import "time"

// Filters are the metadata filters pushed into the store's ranked searches.
// Zero-valued fields do not constrain; CreatedFrom/CreatedTo is what event
// anchoring compiles down to.
type Filters struct {
	Source      string
	RepoRef     string
	DocType     DocType
	CreatedFrom time.Time
	CreatedTo   time.Time
}

// Chunk is an embedding-sized slice of a document body. Its copied document
// metadata is what Filters match at query time.
type Chunk struct {
	DocID     DocID
	Ordinal   int // position within the parent document body
	Text      string
	Source    string
	RepoRef   string
	DocType   DocType
	Author    string
	CreatedAt time.Time
	UpdatedAt time.Time
	ThreadID  string // comment chunks: the thread retrieval rehydrates
}

// ChunkHit is a ranked chunk from one retrieval strategy. Score is the
// backend-native relevance; cross-strategy fusion happens in the service layer.
type ChunkHit struct {
	Chunk
	Score float32
}
