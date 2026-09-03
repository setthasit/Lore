package conformance

import (
	"context"
	"fmt"
	"testing"

	"github.com/setthasit/Lore/internal/entities"
)

type Fixture struct {
	// Docs is the number of documents one full, cursor-less stream yields.
	Docs int

	// ResumeAfterBatch indexes the full-stream batch whose cursor the resume check
	// starts from: that batch was committed, everything after it was not.
	ResumeAfterBatch int

	// ReplayableTypes may reappear below the resume position — immutable records the
	// connector re-yields rather than risk dropping. Any other reappearance is a duplicate.
	ReplayableTypes []entities.DocType
}

// Run asserts the connector contract against fixture. newConnector is called once
// per stream and must open the same unchanged source every time.
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

func assertBatchCursors(t *testing.T, batches []entities.Batch) {
	for i, b := range batches {
		if len(b.Cursor) == 0 {
			t.Errorf("batch %d (%d documents) carries no cursor, so committing it checkpoints nothing", i, len(b.Docs))
		}
	}
}

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

func ids(batches []entities.Batch) []entities.DocID {
	out := make([]entities.DocID, 0, countDocs(batches))
	for _, b := range batches {
		for _, d := range b.Docs {
			out = append(out, d.ID)
		}
	}
	return out
}
