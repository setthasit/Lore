package conform

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

// The five assertions of the conformance suite, named because they are reported
// twice: as subtests when a plugin author runs the suite under `go test`, and as
// Findings when the host runs it against an installed binary.
const (
	checkCursors    = "every batch carries a cursor"
	checkTimestamps = "created_at and updated_at are set"
	checkIdentity   = "every document is fully identified"
	checkIdempotent = "changes is idempotent"
	checkResumable  = "resume from a mid-stream cursor"

	// checkStream is not a sixth assertion but the precondition all five rest
	// on: a stream that fails or that contradicts the fixture leaves nothing to
	// assert, so the suite stops there rather than reporting five derived
	// failures with one cause.
	checkStream = "changes streams to completion"
)

type Fixture struct {
	// Docs is the number of documents one full, cursor-less stream yields. Zero
	// asserts no count: a host verifying a stranger's binary cannot know it,
	// while a plugin's own test always can and so must declare it.
	Docs int

	// ResumeAfterBatch indexes the full-stream batch whose cursor the resume check
	// starts from: that batch was committed, everything after it was not. Zero
	// derives the resume point from the stream, for the same reason Docs may be
	// zero — the shape of a stranger's stream is not knowable in advance.
	ResumeAfterBatch int

	// ReplayableTypes may reappear below the resume position — immutable records the
	// connector re-yields rather than risk dropping. Any other reappearance is a duplicate.
	ReplayableTypes []lore.DocType
}

// Finding is one failed assertion: Check names the assertion, Detail says what
// the connector did instead. An empty slice is a passing plugin.
type Finding struct {
	Check  string
	Detail string
}

// Check runs the suite outside `go test` and returns what failed, so
// `lore plugin verify` certifies a third-party binary through the identical
// code path a plugin author runs locally. newConnector is called once per
// stream and must open the same unchanged source every time.
func Check(newConnector func() lore.Connector, fixture Fixture) []Finding {
	if newConnector == nil {
		return []Finding{{Check: checkStream, Detail: "no connector constructor: there is nothing to certify"}}
	}

	conn := newConnector()
	full, err := collect(conn, nil)
	if err != nil {
		return []Finding{{Check: checkStream, Detail: fmt.Sprintf("%s: full stream: %v", conn.Name(), err)}}
	}
	if fixture.Docs > 0 {
		if n := countDocs(full); n != fixture.Docs {
			return []Finding{{Check: checkStream, Detail: fmt.Sprintf(
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

// Run asserts the connector contract against fixture. newConnector is called once
// per stream and must open the same unchanged source every time.
func Run(t *testing.T, newConnector func() lore.Connector, fixture Fixture) {
	t.Helper()
	if newConnector == nil {
		t.Fatal("conform.Run needs a connector constructor")
	}
	// A plugin's own test knows its fixture, so a missing count is a test bug:
	// every assertion below would hold vacuously over an empty stream.
	if fixture.Docs <= 0 {
		t.Fatalf("fixture declares %d documents: the whole suite would hold vacuously", fixture.Docs)
	}

	findings := Check(newConnector, fixture)
	for _, f := range findings {
		if f.Check == checkStream {
			t.Fatal(f.Detail)
		}
	}

	for _, check := range []string{checkCursors, checkTimestamps, checkIdentity, checkIdempotent, checkResumable} {
		t.Run(check, func(t *testing.T) {
			for _, f := range findings {
				if f.Check == check {
					t.Error(f.Detail)
				}
			}
		})
	}
}

// where distinguishes the full stream from the resumed one, which asserts the
// same rule over a different set of batches.
func batchCursors(batches []lore.Batch, where string) []Finding {
	var findings []Finding
	for i, b := range batches {
		if len(b.Cursor) == 0 {
			findings = append(findings, Finding{checkCursors, fmt.Sprintf(
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
				findings = append(findings, Finding{checkTimestamps, where + ": zero CreatedAt"})
			}
			if d.UpdatedAt.IsZero() {
				findings = append(findings, Finding{checkTimestamps, where + ": zero UpdatedAt"})
			}
		}
	}
	return findings
}

func identity(batches []lore.Batch, source string) []Finding {
	var findings []Finding
	fail := func(format string, args ...any) {
		findings = append(findings, Finding{checkIdentity, fmt.Sprintf(format, args...)})
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
			// The DocID is the join key of the whole index and the host never
			// rebuilds it, so an id that disagrees with the document's own source
			// and type writes into a namespace nothing will look in.
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
		return []Finding{{checkIdempotent, fmt.Sprintf("second full stream: %v", err)}}
	}

	first, again := ids(full), ids(second)
	for i := range min(len(first), len(again)) {
		if first[i] != again[i] {
			return []Finding{{checkIdempotent, fmt.Sprintf(
				"document %d differs between two runs of an unchanged source: %s then %s",
				i, first[i], again[i])}}
		}
	}
	if len(first) != len(again) {
		return []Finding{{checkIdempotent, fmt.Sprintf(
			"two runs of an unchanged source yielded %d and %d documents, agreeing on the first %d",
			len(first), len(again), min(len(first), len(again)))}}
	}
	return nil
}

func resumable(newConnector func() lore.Connector, full []lore.Batch, fixture Fixture) []Finding {
	if len(full) < 2 {
		return []Finding{{checkResumable, fmt.Sprintf(
			"the full stream has %d batch(es): a mid-stream resume needs at least two", len(full))}}
	}
	at := resumePoint(fixture)
	if at < 0 || at >= len(full)-1 {
		return []Finding{{checkResumable, fmt.Sprintf(
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
		return []Finding{{checkResumable, fmt.Sprintf("resuming from the batch %d cursor %v: %v", at, cursor, err)}}
	}

	findings := batchCursors(resumed, "resumed ")
	fail := func(format string, args ...any) {
		findings = append(findings, Finding{checkResumable, fmt.Sprintf(format, args...)})
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

// A host verifying a stranger's binary knows nothing about the shape of its
// stream, so an unset ResumeAfterBatch derives the resume point: the first
// batch that has a successor, the earliest position at which "replay from
// batch n yields batch n+1 onward" is observable at all.
func resumePoint(fixture Fixture) int {
	if fixture.ResumeAfterBatch != 0 {
		return fixture.ResumeAfterBatch
	}
	return 0
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
