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

	CheckStream      CheckName = "changes streams to completion"
	CheckStreamError CheckName = "an error ends the stream"
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

// Result records one run: a finding is attributable only to a check in Ran.
type Result struct {
	Findings []Finding
	Ran      []CheckName
	Skipped  []Skip
}

type Skip struct {
	Check  CheckName
	Reason string
}

type stage struct {
	check  CheckName
	skip   func(batches []lore.Batch) string
	assert func() []Finding
}

func noDocument(batches []lore.Batch) string {
	if countDocs(batches) > 0 {
		return ""
	}
	return "the stream carried no document to check"
}

// Check runs the suite outside `go test`. newConnector is called once per
// stream and must open the same unchanged source every time. A cancelled ctx
// returns early with the checks it reached, so check ctx.Err() before trusting
// the Result. Pass nil for unconfigured: a non-nil predicate skips, rather
// than faults, a stream that fails before its first batch with an error the
// predicate accepts, and waives the resume check below two batches.
func Check(ctx context.Context, newConnector func() lore.Connector, fixture Fixture, unconfigured func(error) bool) Result {
	var result Result
	var watch errorWatch

	source, full, failed := fullStream(ctx, newConnector, fixture, &watch)
	stages := []stage{
		{
			check: CheckCursors,
			skip: func(batches []lore.Batch) string {
				if len(batches) > 0 {
					return ""
				}
				return "the stream carried no batch whose cursor could be checked"
			},
			assert: func() []Finding { return batchCursors(full, "") },
		},
		{
			check:  CheckTimestamps,
			skip:   noDocument,
			assert: func() []Finding { return timestamps(full) },
		},
		{
			check:  CheckIdentity,
			skip:   noDocument,
			assert: func() []Finding { return identity(full, source) },
		},
		{
			check:  CheckIdempotent,
			assert: func() []Finding { return idempotent(ctx, newConnector, full, &watch) },
		},
		{
			check: CheckResumable,
			skip: func(batches []lore.Batch) string {
				if unconfigured == nil || !tooShortToResume(batches) {
					return ""
				}
				return fmt.Sprintf(
					"the stream an unconfigured source gave has %d batch(es), and %s",
					len(batches), resumeNeedsTwoBatches)
			},
			assert: func() []Finding { return resumable(ctx, newConnector, full, fixture, &watch) },
		},
	}

	if failed != nil {
		if unconfigured != nil && !failed.opened && unconfigured(failed.err) {
			result.recordSkip(CheckStream, fmt.Sprintf(
				"no configuration was supplied, and the source declined to stream without it: %v", failed.err))
			result.recordErrorWatch(&watch,
				"the source declined to stream, so it yielded no error the suite could judge as ending a stream")
		} else {
			result.recordRan(CheckStream, []Finding{failed.finding})
			result.recordErrorWatch(&watch, "")
		}
		for _, s := range stages {
			result.recordSkip(s.check, failed.reason)
		}
		return result
	}

	result.recordRan(CheckStream, nil)
	for _, s := range stages {
		if ctx.Err() != nil {
			break
		}
		if s.skip != nil {
			if reason := s.skip(full); reason != "" {
				result.recordSkip(s.check, reason)
				continue
			}
		}
		result.recordRan(s.check, s.assert())
	}
	result.recordErrorWatch(&watch, "")
	return result
}

const resumeNeedsTwoBatches = "a mid-stream resume needs at least two"

func tooShortToResume(batches []lore.Batch) bool { return len(batches) < 2 }

func (r *Result) recordRan(check CheckName, findings []Finding) {
	r.Ran = append(r.Ran, check)
	r.Findings = append(r.Findings, findings...)
}

func (r *Result) recordSkip(check CheckName, reason string) {
	r.Skipped = append(r.Skipped, Skip{Check: check, Reason: reason})
}

func (r *Result) recordErrorWatch(w *errorWatch, declined string) {
	switch {
	case len(w.findings) > 0:
		r.recordRan(CheckStreamError, w.findings)
	case declined != "":
		r.recordSkip(CheckStreamError, declined)
	default:
		r.recordRan(CheckStreamError, nil)
	}
}

type errorWatch struct {
	findings []Finding
}

func (w *errorWatch) fail(stream, format string, args ...any) {
	w.findings = append(w.findings, Finding{CheckStreamError, stream + ": " + fmt.Sprintf(format, args...)})
}

func (w *errorWatch) beside(stream string, batch lore.Batch) {
	if len(batch.Docs) == 0 && len(batch.Cursor) == 0 {
		return
	}
	w.fail(stream,
		"an error arrived in the same yield as a batch carrying %d documents and %d cursor keys: a consumer drops that batch, so neither its documents nor its cursor survive the error",
		len(batch.Docs), len(batch.Cursor))
}

func (w *errorWatch) after(stream string, batch lore.Batch, err error) {
	if err != nil {
		w.fail(stream, "the stream yielded another error after the first: an error ends the stream, so nothing after it reaches a consumer")
		return
	}
	w.fail(stream,
		"the stream yielded a batch carrying %d documents after an error: an error ends the stream, so nothing after it reaches a consumer",
		len(batch.Docs))
}

type streamFailure struct {
	finding Finding
	err     error
	opened  bool
	reason  string
}

func fullStream(ctx context.Context, newConnector func() lore.Connector, fixture Fixture, watch *errorWatch) (string, []lore.Batch, *streamFailure) {
	conn := newConnector()

	full, delivered, err := collect(ctx, conn, nil, watch, "full stream")
	if err != nil {
		failed := &streamFailure{
			finding: Finding{CheckStream, fmt.Sprintf("%s: full stream: %v", conn.Name(), err)},
			err:     err,
			opened:  delivered > 0,
			reason:  "no complete batch reached the suite before the stream failed",
		}
		if failed.opened {
			failed.reason = fmt.Sprintf(
				"the stream broke after %d batch(es), so what it delivered is not a complete sample", delivered)
		}
		return conn.Name(), nil, failed
	}

	if fixture.Docs > 0 {
		if n := countDocs(full); n != fixture.Docs {
			return conn.Name(), nil, &streamFailure{
				finding: Finding{CheckStream, fmt.Sprintf(
					"%s: full stream yielded %d documents in %d batches, fixture declares %d",
					conn.Name(), n, len(full), fixture.Docs)},
				opened: true,
				reason: "the stream it reads completed but disagreed with the declared document count",
			}
		}
	}
	return conn.Name(), full, nil
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

	result := Check(t.Context(), newConnector, fixture, nil)
	reportSkipsAndFindingsBeforeTheFatal(t, result)

	for _, check := range result.Ran {
		if check == CheckStream {
			continue
		}
		t.Run(string(check), func(t *testing.T) {
			for _, f := range result.Findings {
				if f.Check == check {
					t.Error(f.Detail)
				}
			}
		})
	}
}

type failureReporter interface {
	Helper()
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
	Fatal(args ...any)
}

func reportSkipsAndFindingsBeforeTheFatal(t failureReporter, result Result) {
	t.Helper()
	for _, skip := range result.Skipped {
		t.Logf("%s — %s", skip.Check, skip.Reason)
	}

	failed := ""
	for _, f := range result.Findings {
		if f.Check == CheckStream {
			failed = f.Detail
			break
		}
	}
	if failed == "" {
		return
	}
	for _, f := range result.Findings {
		if f.Check == CheckStreamError {
			t.Errorf("%s: %s", f.Check, f.Detail)
		}
	}
	t.Fatal(failed)
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

func idempotent(ctx context.Context, newConnector func() lore.Connector, full []lore.Batch, watch *errorWatch) []Finding {
	second, _, err := collect(ctx, newConnector(), nil, watch, "second full stream")
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

func resumable(ctx context.Context, newConnector func() lore.Connector, full []lore.Batch, fixture Fixture, watch *errorWatch) []Finding {
	if tooShortToResume(full) {
		return []Finding{{CheckResumable, fmt.Sprintf(
			"the full stream has %d batch(es): %s", len(full), resumeNeedsTwoBatches)}}
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
	resumed, _, err := collect(ctx, newConnector(), cursor, watch, "resumed stream")
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

func collect(ctx context.Context, c lore.Connector, cursor lore.Cursor, watch *errorWatch, stream string) ([]lore.Batch, int, error) {
	var batches []lore.Batch
	var failure error
	for batch, err := range c.Changes(ctx, cursor) {
		if failure != nil {
			watch.after(stream, batch, err)
			break
		}
		if err != nil {
			failure = err
			watch.beside(stream, batch)
			continue
		}
		batches = append(batches, batch)
	}
	if failure != nil {
		return nil, len(batches), failure
	}
	return batches, len(batches), nil
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
