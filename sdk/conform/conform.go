package conform

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

type CheckName string

const (
	CheckCursors    CheckName = "every batch carries a cursor"
	CheckTimestamps CheckName = "created_at and updated_at are set"
	CheckIdentity   CheckName = "every document is fully identified"
	CheckIdempotent CheckName = "changes is idempotent"
	CheckResumable  CheckName = "resume from a mid-stream cursor"

	CheckStream CheckName = "changes streams to completion"
)

type Fixture struct {
	// Docs is the number of documents one full, cursor-less stream yields. Zero
	// asserts no count in Check; Run requires it.
	Docs int

	// ResumeAfterBatch indexes the full-stream batch whose cursor the resume
	// check starts from; zero resumes after batch 0.
	ResumeAfterBatch int

	// ReplayableTypes may reappear below the resume position: immutable records
	// the connector re-yields rather than drop. Any other reappearance is a duplicate.
	ReplayableTypes []lore.DocType
}

// Finding is one failed assertion; an empty slice is a passing plugin.
type Finding struct {
	Check  CheckName
	Detail string
}

// Check runs the suite outside `go test`. newConnector is called once per
// stream and must open the same unchanged source every time.
func Check(newConnector func() lore.Connector, fixture Fixture) []Finding {
	conn := newConnector()
	full, err := collect(conn, nil)
	if err != nil {
		return []Finding{{Check: CheckStream, Detail: fmt.Sprintf("%s: full stream: %v", conn.Name(), err)}}
	}
	if fixture.Docs > 0 {
		if n := countDocs(full); n != fixture.Docs {
			return []Finding{{Check: CheckStream, Detail: fmt.Sprintf(
				"%s: full stream yielded %d documents in %d batches, fixture declares %d",
				conn.Name(), n, len(full), fixture.Docs)}}
		}
	}

	var findings []Finding
	findings = append(findings, batchCursors(full, "")...)
	findings = append(findings, timestamps(full)...)
	findings = append(findings, identity(full, conn.Name())...)
	findings = append(findings, idempotent(newConnector, full)...)
	findings = append(findings, resumable(newConnector, full, fixture)...)
	return findings
}

// Run is Check as a `go test` subtest tree; newConnector is called once per
// stream and must open the same unchanged source every time.
func Run(t *testing.T, newConnector func() lore.Connector, fixture Fixture) {
	t.Helper()
	if newConnector == nil {
		t.Fatal("conform.Run needs a connector constructor")
	}
	if fixture.Docs <= 0 {
		t.Fatalf("fixture declares %d documents: the whole suite would hold vacuously", fixture.Docs)
	}

	findings := Check(newConnector, fixture)
	for _, f := range findings {
		if f.Check == CheckStream {
			t.Fatal(f.Detail)
		}
	}

	for _, check := range []CheckName{CheckCursors, CheckTimestamps, CheckIdentity, CheckIdempotent, CheckResumable} {
		t.Run(string(check), func(t *testing.T) {
			for _, f := range findings {
				if f.Check == check {
					t.Error(f.Detail)
				}
			}
		})
	}
}

func batchCursors(batches []lore.Batch, where string) []Finding {
	var findings []Finding
	for i, b := range batches {
		if len(b.Cursor) == 0 {
			findings = append(findings, Finding{CheckCursors, fmt.Sprintf(
				"%sbatch %d (%d documents) carries no cursor, so committing it checkpoints nothing",
				where, i, len(b.Docs))})
		}
	}
	return findings
}

func timestamps(batches []lore.Batch) []Finding {
	var findings []Finding
	for i, b := range batches {
		for j, d := range b.Docs {
			where := fmt.Sprintf("batch %d document %d (%s)", i, j, d.ID)
			if d.CreatedAt.IsZero() {
				findings = append(findings, Finding{CheckTimestamps, where + ": zero CreatedAt"})
			}
			if d.UpdatedAt.IsZero() {
				findings = append(findings, Finding{CheckTimestamps, where + ": zero UpdatedAt"})
			}
		}
	}
	return findings
}

func identity(batches []lore.Batch, source string) []Finding {
	var findings []Finding
	fail := func(format string, args ...any) {
		findings = append(findings, Finding{CheckIdentity, fmt.Sprintf(format, args...)})
	}

	for i, b := range batches {
		for j, d := range b.Docs {
			if d.ID == "" {
				fail("batch %d document %d has an empty DocID: %+v", i, j, d)
				continue
			}
			where := fmt.Sprintf("batch %d: %s", i, d.ID)
			switch {
			case d.Source == "":
				fail("%s: empty Source", where)
			case d.Source != source:
				fail("%s: Source %q, want the connector name %q", where, d.Source, source)
			}
			if d.Type == "" {
				fail("%s: empty Type", where)
			}
			if d.URL == "" {
				fail("%s: empty URL, so the document cannot be cited", where)
			}
			if prefix := d.Source + ":" + string(d.Type) + ":"; d.Source != "" && d.Type != "" {
				if external, ok := strings.CutPrefix(string(d.ID), prefix); !ok || external == "" {
					fail("%s: DocID is not %q plus a non-empty external id", where, prefix)
				}
			}
		}
	}
	return findings
}

func idempotent(newConnector func() lore.Connector, full []lore.Batch) []Finding {
	second, err := collect(newConnector(), nil)
	if err != nil {
		return []Finding{{CheckIdempotent, fmt.Sprintf("second full stream: %v", err)}}
	}

	first, again := ids(full), ids(second)
	for i := range min(len(first), len(again)) {
		if first[i] != again[i] {
			return []Finding{{CheckIdempotent, fmt.Sprintf(
				"document %d differs between two runs of an unchanged source: %s then %s",
				i, first[i], again[i])}}
		}
	}
	if len(first) != len(again) {
		return []Finding{{CheckIdempotent, fmt.Sprintf(
			"two runs of an unchanged source yielded %d and %d documents, agreeing on the first %d",
			len(first), len(again), min(len(first), len(again)))}}
	}
	return nil
}

func resumable(newConnector func() lore.Connector, full []lore.Batch, fixture Fixture) []Finding {
	if len(full) < 2 {
		return []Finding{{CheckResumable, fmt.Sprintf(
			"the full stream has %d batch(es): a mid-stream resume needs at least two", len(full))}}
	}
	at := fixture.ResumeAfterBatch
	if at < 0 || at >= len(full)-1 {
		return []Finding{{CheckResumable, fmt.Sprintf(
			"ResumeAfterBatch %d has to name a batch of the %d-batch stream with at least one batch after it",
			at, len(full))}}
	}

	inFull := make(map[lore.DocID]bool, countDocs(full))
	for _, b := range full {
		for _, d := range b.Docs {
			inFull[d.ID] = true
		}
	}
	committed := make(map[lore.DocID]bool, countDocs(full[:at+1]))
	for _, b := range full[:at+1] {
		for _, d := range b.Docs {
			committed[d.ID] = true
		}
	}

	cursor := full[at].Cursor
	resumed, err := collect(newConnector(), cursor)
	if err != nil {
		return []Finding{{CheckResumable, fmt.Sprintf("resuming from the batch %d cursor %v: %v", at, cursor, err)}}
	}

	findings := batchCursors(resumed, "resumed ")
	fail := func(format string, args ...any) {
		findings = append(findings, Finding{CheckResumable, fmt.Sprintf(format, args...)})
	}

	replayable := make(map[lore.DocType]bool, len(fixture.ReplayableTypes))
	for _, dt := range fixture.ReplayableTypes {
		replayable[dt] = true
	}

	resumedIDs := make(map[lore.DocID]bool, countDocs(resumed))
	for i, b := range resumed {
		for _, d := range b.Docs {
			resumedIDs[d.ID] = true
			switch {
			case !inFull[d.ID]:
				fail("%s (resumed batch %d) is absent from the full stream of the same unchanged source", d.ID, i)
			case committed[d.ID] && !replayable[d.Type]:
				fail("%s (resumed batch %d) is a duplicate: it precedes the batch %d cursor %v, and type %q is not declared replayable (%v)",
					d.ID, i, at, cursor, d.Type, fixture.ReplayableTypes)
			}
		}
	}
	for i, b := range full {
		for _, d := range b.Docs {
			if !committed[d.ID] && !resumedIDs[d.ID] {
				fail("%s (batch %d) is lost: the batch %d cursor %v does not cover it and the resumed stream does not yield it",
					d.ID, i, at, cursor)
			}
		}
	}
	return findings
}

func collect(c lore.Connector, cursor lore.Cursor) ([]lore.Batch, error) {
	var batches []lore.Batch
	for batch, err := range c.Changes(context.Background(), cursor) {
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	return batches, nil
}

func countDocs(batches []lore.Batch) int {
	n := 0
	for _, b := range batches {
		n += len(b.Docs)
	}
	return n
}

func ids(batches []lore.Batch) []lore.DocID {
	out := make([]lore.DocID, 0, countDocs(batches))
	for _, b := range batches {
		for _, d := range b.Docs {
			out = append(out, d.ID)
		}
	}
	return out
}
