package conform_test

import (
	"context"
	"errors"
	"iter"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
)

// The check names are part of the API: `lore plugin verify` prints them to a
// plugin author who has to find the assertion in the document from the name
// alone.
const (
	cursors    = "every batch carries a cursor"
	timestamps = "created_at and updated_at are set"
	identity   = "every document is fully identified"
	idempotent = "changes is idempotent"
	resumable  = "resume from a mid-stream cursor"
	stream     = "changes streams to completion"
)

// stub is a connector whose whole behaviour is one function, so a test can be
// exactly as wrong as the rule it is about.
type stub struct {
	name   string
	stream func(cursor lore.Cursor) ([]lore.Batch, error)
}

func (s stub) Name() string { return s.name }

func (s stub) Changes(_ context.Context, cursor lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) {
		batches, err := s.stream(cursor)
		if err != nil {
			yield(lore.Batch{}, err)
			return
		}
		for _, batch := range batches {
			if !yield(batch, nil) {
				return
			}
		}
	}
}

func doc(index int) lore.Document {
	when := time.Date(2026, time.August, 10+index, 9, 0, 0, 0, time.UTC)
	external := strconv.Itoa(index)
	return lore.Document{
		ID:        lore.NewDocID("stub", lore.DocTypeTicket, external),
		Source:    "stub",
		Type:      lore.DocTypeTicket,
		Title:     "ticket " + external,
		URL:       "https://tickets.example.test/" + external,
		CreatedAt: when,
		UpdatedAt: when,
	}
}

// conformant streams four documents in two batches and honours its cursor,
// which is the behaviour every case below breaks in exactly one way.
func conformant(cursor lore.Cursor) ([]lore.Batch, error) {
	after := 0
	if raw, ok := cursor["after"]; ok {
		after, _ = strconv.Atoi(raw)
	}

	var batches []lore.Batch
	for _, pair := range [][]int{{1, 2}, {3, 4}} {
		var docs []lore.Document
		for _, index := range pair {
			if index > after {
				docs = append(docs, doc(index))
			}
		}
		batches = append(batches, lore.Batch{Docs: docs, Cursor: lore.Cursor{"after": strconv.Itoa(pair[len(pair)-1])}})
	}
	return batches, nil
}

func newStub(stream func(lore.Cursor) ([]lore.Batch, error)) func() lore.Connector {
	return func() lore.Connector { return stub{name: "stub", stream: stream} }
}

func checkNames(findings []conform.Finding) []string {
	var names []string
	for _, f := range findings {
		names = append(names, f.Check)
	}
	return names
}

func TestCheckPassesAConformantConnector(t *testing.T) {
	for _, f := range conform.Check(newStub(conformant), conform.Fixture{Docs: 4}) {
		t.Errorf("%s: %s", f.Check, f.Detail)
	}
}

// A host verifying a stranger's binary knows neither its document count nor the
// shape of its stream, so both fixture facts are optional.
func TestCheckWithoutFixtureFactsStillCertifies(t *testing.T) {
	for _, f := range conform.Check(newStub(conformant), conform.Fixture{}) {
		t.Errorf("%s: %s", f.Check, f.Detail)
	}
}

func TestCheckReportsTheFailedAssertion(t *testing.T) {
	tests := map[string]struct {
		stream func(lore.Cursor) ([]lore.Batch, error)
		want   string
		detail string
	}{
		"a batch with no cursor checkpoints nothing": {
			stream: func(cursor lore.Cursor) ([]lore.Batch, error) {
				batches, _ := conformant(cursor)
				batches[0].Cursor = nil
				return batches, nil
			},
			want:   cursors,
			detail: "carries no cursor",
		},
		"a document with no timestamps cannot be ordered": {
			stream: func(cursor lore.Cursor) ([]lore.Batch, error) {
				batches, _ := conformant(cursor)
				if len(batches[0].Docs) > 0 {
					batches[0].Docs[0].CreatedAt = time.Time{}
				}
				return batches, nil
			},
			want:   timestamps,
			detail: "zero CreatedAt",
		},
		"a document with no URL cannot be cited": {
			stream: func(cursor lore.Cursor) ([]lore.Batch, error) {
				batches, _ := conformant(cursor)
				if len(batches[0].Docs) > 0 {
					batches[0].Docs[0].URL = ""
				}
				return batches, nil
			},
			want:   identity,
			detail: "empty URL",
		},
		"an id that disagrees with its own parts lands in a namespace nothing reads": {
			stream: func(cursor lore.Cursor) ([]lore.Batch, error) {
				batches, _ := conformant(cursor)
				if len(batches[0].Docs) > 0 {
					batches[0].Docs[0].ID = "made-up"
				}
				return batches, nil
			},
			want:   identity,
			detail: "plus a non-empty external id",
		},
		"a source that renames itself writes into another instance's namespace": {
			stream: func(cursor lore.Cursor) ([]lore.Batch, error) {
				batches, _ := conformant(cursor)
				if len(batches[0].Docs) > 0 {
					batches[0].Docs[0].Source = "somebody-else"
				}
				return batches, nil
			},
			want:   identity,
			detail: "want the connector name",
		},
		"a second run that yields something else is not idempotent": {
			stream: func() func(lore.Cursor) ([]lore.Batch, error) {
				runs := 0
				return func(cursor lore.Cursor) ([]lore.Batch, error) {
					batches, _ := conformant(cursor)
					runs++
					if runs > 1 && len(cursor) == 0 {
						batches[1].Docs = nil
					}
					return batches, nil
				}
			}(),
			want:   idempotent,
			detail: "yielded 4 and 2 documents",
		},
		"a connector that ignores its cursor replays committed documents": {
			stream: func(cursor lore.Cursor) ([]lore.Batch, error) {
				return conformant(nil)
			},
			want:   resumable,
			detail: "is a duplicate",
		},
		"a resume that skips past the cursor loses documents": {
			stream: func(cursor lore.Cursor) ([]lore.Batch, error) {
				batches, _ := conformant(cursor)
				if len(cursor) > 0 {
					batches[len(batches)-1].Docs = nil
				}
				return batches, nil
			},
			want:   resumable,
			detail: "is lost",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			findings := conform.Check(newStub(tt.stream), conform.Fixture{Docs: 4})
			if len(findings) == 0 {
				t.Fatal("the suite passed a connector that breaks a rule")
			}

			found := false
			for _, f := range findings {
				if f.Check == tt.want && strings.Contains(f.Detail, tt.detail) {
					found = true
				}
			}
			if !found {
				t.Errorf("findings %v do not report %q containing %q; details: %+v",
					checkNames(findings), tt.want, tt.detail, findings)
			}
		})
	}
}

func TestReplayableTypesAreAllowedBackIntoTheStream(t *testing.T) {
	// A record whose timestamp ties with the cursor is re-yielded rather than
	// risked, which is a declared property and not a duplicate.
	replays := func(cursor lore.Cursor) ([]lore.Batch, error) {
		batches, _ := conformant(cursor)
		if len(cursor) > 0 {
			batches[len(batches)-1].Docs = append([]lore.Document{doc(2)}, batches[len(batches)-1].Docs...)
		}
		return batches, nil
	}

	if findings := conform.Check(newStub(replays), conform.Fixture{Docs: 4}); len(findings) == 0 {
		t.Error("an undeclared replay was accepted")
	}
	findings := conform.Check(newStub(replays), conform.Fixture{
		Docs:            4,
		ReplayableTypes: []lore.DocType{lore.DocTypeTicket},
	})
	for _, f := range findings {
		t.Errorf("%s: %s", f.Check, f.Detail)
	}
}

func TestAStreamThatFailsIsReportedOnce(t *testing.T) {
	broken := func(lore.Cursor) ([]lore.Batch, error) { return nil, errors.New("token expired") }

	findings := conform.Check(newStub(broken), conform.Fixture{Docs: 4})
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one failure the others all derive from", findings)
	}
	if findings[0].Check != stream || !strings.Contains(findings[0].Detail, "token expired") {
		t.Errorf("finding = %+v, want the stream failure carrying the connector's error", findings[0])
	}
}

func TestADeclaredCountThatDisagreesIsAFailure(t *testing.T) {
	findings := conform.Check(newStub(conformant), conform.Fixture{Docs: 5})
	if len(findings) != 1 || findings[0].Check != stream {
		t.Fatalf("findings = %+v, want one stream failure about the count", findings)
	}
	if !strings.Contains(findings[0].Detail, "fixture declares 5") {
		t.Errorf("finding %q does not name the declared count", findings[0].Detail)
	}
}

func TestASingleBatchStreamCannotProveResumability(t *testing.T) {
	single := func(lore.Cursor) ([]lore.Batch, error) {
		return []lore.Batch{{Docs: []lore.Document{doc(1)}, Cursor: lore.Cursor{"after": "1"}}}, nil
	}

	findings := conform.Check(newStub(single), conform.Fixture{Docs: 1})
	if len(findings) != 1 || findings[0].Check != resumable {
		t.Fatalf("findings = %+v, want the resume check to report that it could not run", findings)
	}
}

func TestNoConnectorIsAFinding(t *testing.T) {
	findings := conform.Check(nil, conform.Fixture{})
	if len(findings) != 1 || findings[0].Check != stream {
		t.Fatalf("findings = %+v, want one finding about having nothing to certify", findings)
	}
}
