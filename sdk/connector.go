package lore

import (
	"context"
	"iter"
)

// A connector emits RawRef and never resolves a reference; the host does that.
type Connector interface {
	// Name is the instance id: the cursor key, every Document.Source, and the
	// DocID prefix.
	Name() string

	// Batches carry documents modified since cursor, oldest-first; a nil cursor
	// streams everything. Must be resumable and idempotent. An error ends the
	// stream: its batch is not committed and the cursor stays where the last
	// committed batch left it.
	Changes(ctx context.Context, cursor Cursor) iter.Seq2[Batch, error]
}
