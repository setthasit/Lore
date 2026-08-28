package conformance

import (
	"context"
	"fmt"
	"testing"

	"lore/internal/entities"
)

// Fixture declares what the connector's test source holds. It exists so the
// suite can tell a satisfied contract from a source that yields nothing: every
// assertion below holds vacuously over an empty stream.
type Fixture struct {
	// Docs is the number of documents one full, cursor-less stream yields.
	Docs int

	// ResumeAfterBatch indexes the full-stream batch whose cursor the
	// resumability check resumes from — the crash point being simulated: that
	// batch was committed, everything after it was not. Zero, the first batch,
	// exercises the longest replay window; the index has to leave at least one
	// batch behind it.
	ResumeAfterBatch int

	// ReplayableTypes lists the DocTypes the connector's package doc declares as
	// replayed on resume: immutable records it re-yields rather than risk
	// dropping, such as a commit whose watermark ties with the cursor's second.
	// Only these types may reappear below a resume position, where upsert by
	// DocID absorbs them; any other reappearance is a duplicate.
	ReplayableTypes []entities.DocType
}

// Run asserts the connector contract against fixture. newConnector is called
// once per stream and must return a connector over the same unchanged source
// every time; a connector that caches nothing between streams may return the
// same instance.
func Run(t *testing.T, newConnector func() entities.Connector, fixture Fixture) {
	t.Helper()
	if newConnector == nil {
		t.Fatal("conformance.Run needs a connector constructor")
	}
	if fixture.Docs <= 0 {
		t.Fatalf("fixture declares %d documents: the whole suite would hold vacuously", fixture.Docs)
	}

	conn := newConnector()
	full, err := collect(conn, nil)
	if err != nil {
		t.Fatalf("%s: full stream: %v", conn.Name(), err)
	}
	if n := countDocs(full); n != fixture.Docs {
		t.Fatalf("%s: full stream yielded %d documents in %d batches, fixture declares %d",
			conn.Name(), n, len(full), fixture.Docs)
	}

	t.Run("every batch carries a cursor", func(t *testing.T) { assertBatchCursors(t, full) })
	t.Run("every document is fully identified", func(t *testing.T) { assertIdentity(t, full, conn.Name()) })
	t.Run("changes is idempotent", func(t *testing.T) { assertIdempotent(t, newConnector, full) })
	t.Run("resume from a mid-stream cursor", func(t *testing.T) { assertResumable(t, newConnector, full, fixture) })
}

// assertBatchCursors checks that every batch is checkpointable. The batch is the
// checkpoint unit: a batch whose cursor says nothing cannot be committed without
// re-reading the stream from the start after a crash.
func assertBatchCursors(t *testing.T, batches []entities.Batch) {
	for i, b := range batches {
		if len(b.Cursor) == 0 {
			t.Errorf("batch %d (%d documents) carries no cursor, so committing it checkpoints nothing", i, len(b.Docs))
		}
	}
}

// assertIdentity checks the fields every downstream stage needs: an id to upsert
// by, a source and type to route on, a URL to cite, and both timestamps.
func assertIdentity(t *testing.T, batches []entities.Batch, source string) {
	for i, b := range batches {
		for j, d := range b.Docs {
			if d.ID == "" {
				t.Errorf("batch %d document %d has an empty DocID: %+v", i, j, d)
				continue
			}
			where := fmt.Sprintf("batch %d: %s", i, d.ID)
			switch {
			case d.Source == "":
				t.Errorf("%s: empty Source", where)
			case d.Source != source:
				t.Errorf("%s: Source %q, want the connector name %q", where, d.Source, source)
			}
			if d.Type == "" {
				t.Errorf("%s: empty Type", where)
			}
			if d.URL == "" {
				t.Errorf("%s: empty URL, so the document cannot be cited", where)
			}
			if d.CreatedAt.IsZero() {
				t.Errorf("%s: zero CreatedAt", where)
			}
			if d.UpdatedAt.IsZero() {
				t.Errorf("%s: zero UpdatedAt", where)
			}
		}
	}
}

// assertIdempotent checks that an unchanged source streams identically twice.
// Without this a re-run after a failed sync round could not be trusted to
// reproduce what the interrupted round had already committed.
func assertIdempotent(t *testing.T, newConnector func() entities.Connector, full []entities.Batch) {
	second, err := collect(newConnector(), nil)
	if err != nil {
		t.Fatalf("second full stream: %v", err)
	}

	first, again := ids(full), ids(second)
	for i := range min(len(first), len(again)) {
		if first[i] != again[i] {
			t.Fatalf("document %d differs between two runs of an unchanged source: %s then %s",
				i, first[i], again[i])
		}
	}
	if len(first) != len(again) {
		t.Fatalf("two runs of an unchanged source yielded %d and %d documents, agreeing on the first %d",
			len(first), len(again), min(len(first), len(again)))
	}
}

// assertResumable simulates a crash after batch fixture.ResumeAfterBatch was
// committed: that batch's cursor is durable, the batches after it are not. The
// documents already covered plus the resumed stream have to add up to exactly
// the full stream — no document lost, none yielded twice unless its type is
// declared replayable.
func assertResumable(t *testing.T, newConnector func() entities.Connector, full []entities.Batch, fixture Fixture) {
	if len(full) < 2 {
		t.Fatalf("the full stream has %d batch(es): a mid-stream resume needs at least two", len(full))
	}
	at := fixture.ResumeAfterBatch
	if at < 0 || at >= len(full)-1 {
		t.Fatalf("ResumeAfterBatch %d has to name a batch of the %d-batch stream with at least one batch after it",
			at, len(full))
	}

	inFull := make(map[entities.DocID]bool, countDocs(full))
	for _, b := range full {
		for _, d := range b.Docs {
			inFull[d.ID] = true
		}
	}
	committed := make(map[entities.DocID]bool, countDocs(full[:at+1]))
	for _, b := range full[:at+1] {
		for _, d := range b.Docs {
			committed[d.ID] = true
		}
	}

	cursor := full[at].Cursor
	resumed, err := collect(newConnector(), cursor)
	if err != nil {
		t.Fatalf("resuming from the batch %d cursor %v: %v", at, cursor, err)
	}

	assertBatchCursors(t, resumed)

	replayable := make(map[entities.DocType]bool, len(fixture.ReplayableTypes))
	for _, dt := range fixture.ReplayableTypes {
		replayable[dt] = true
	}

	resumedIDs := make(map[entities.DocID]bool, countDocs(resumed))
	for i, b := range resumed {
		for _, d := range b.Docs {
			resumedIDs[d.ID] = true
			switch {
			case !inFull[d.ID]:
				t.Errorf("%s (resumed batch %d) is absent from the full stream of the same unchanged source", d.ID, i)
			case committed[d.ID] && !replayable[d.Type]:
				t.Errorf("%s (resumed batch %d) is a duplicate: it precedes the batch %d cursor %v, and type %q is not declared replayable (%v)",
					d.ID, i, at, cursor, d.Type, fixture.ReplayableTypes)
			}
		}
	}
	for i, b := range full {
		for _, d := range b.Docs {
			if !committed[d.ID] && !resumedIDs[d.ID] {
				t.Errorf("%s (batch %d) is lost: the batch %d cursor %v does not cover it and the resumed stream does not yield it",
					d.ID, i, at, cursor)
			}
		}
	}
}

// collect drains a stream to completion. An error ends the read the way the
// orchestrator ends a round: nothing past it is trusted.
func collect(c entities.Connector, cursor entities.Cursor) ([]entities.Batch, error) {
	var batches []entities.Batch
	for batch, err := range c.Changes(context.Background(), cursor) {
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	return batches, nil
}

func countDocs(batches []entities.Batch) int {
	n := 0
	for _, b := range batches {
		n += len(b.Docs)
	}
	return n
}

// ids flattens a stream to its document ids in yield order.
func ids(batches []entities.Batch) []entities.DocID {
	out := make([]entities.DocID, 0, countDocs(batches))
	for _, b := range batches {
		for _, d := range b.Docs {
			out = append(out, d.ID)
		}
	}
	return out
}
