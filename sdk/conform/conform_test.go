package conform_test

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
)

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

func checkNames(findings []conform.Finding) []conform.CheckName {
	var names []conform.CheckName
	for _, f := range findings {
		names = append(names, f.Check)
	}
	return names
}

func TestCheckPassesAConformantConnector(t *testing.T) {
	for _, f := range conform.Check(t.Context(), newStub(conformant), conform.Fixture{Docs: 4}, nil).Findings {
		t.Errorf("%s: %s", f.Check, f.Detail)
	}
}

func TestCheckWithoutFixtureFactsStillCertifies(t *testing.T) {
	for _, f := range conform.Check(t.Context(), newStub(conformant), conform.Fixture{}, nil).Findings {
		t.Errorf("%s: %s", f.Check, f.Detail)
	}
}

func TestCheckReportsTheFailedAssertion(t *testing.T) {
	tests := map[string]struct {
		stream func(lore.Cursor) ([]lore.Batch, error)
		want   conform.CheckName
		detail string
	}{
		"a batch with no cursor checkpoints nothing": {
			stream: func(cursor lore.Cursor) ([]lore.Batch, error) {
				batches, _ := conformant(cursor)
				batches[0].Cursor = nil
				return batches, nil
			},
			want:   conform.CheckCursors,
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
			want:   conform.CheckTimestamps,
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
			want:   conform.CheckIdentity,
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
			want:   conform.CheckIdentity,
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
			want:   conform.CheckIdentity,
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
			want:   conform.CheckIdempotent,
			detail: "yielded 4 and 2 documents",
		},
		"a connector that ignores its cursor replays committed documents": {
			stream: func(cursor lore.Cursor) ([]lore.Batch, error) {
				return conformant(nil)
			},
			want:   conform.CheckResumable,
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
			want:   conform.CheckResumable,
			detail: "is lost",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			findings := conform.Check(t.Context(), newStub(tt.stream), conform.Fixture{Docs: 4}, nil).Findings
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
	// A record whose timestamp ties with the cursor is re-yielded rather than risked, which is a declared property.
	replays := func(cursor lore.Cursor) ([]lore.Batch, error) {
		batches, _ := conformant(cursor)
		if len(cursor) > 0 {
			batches[len(batches)-1].Docs = append([]lore.Document{doc(2)}, batches[len(batches)-1].Docs...)
		}
		return batches, nil
	}

	if findings := conform.Check(t.Context(), newStub(replays), conform.Fixture{Docs: 4}, nil).Findings; len(findings) == 0 {
		t.Error("an undeclared replay was accepted")
	}
	findings := conform.Check(t.Context(), newStub(replays), conform.Fixture{
		Docs:            4,
		ReplayableTypes: []lore.DocType{lore.DocTypeTicket},
	}, nil).Findings
	for _, f := range findings {
		t.Errorf("%s: %s", f.Check, f.Detail)
	}
}

func TestAStreamThatFailsIsReportedOnce(t *testing.T) {
	broken := func(lore.Cursor) ([]lore.Batch, error) { return nil, errors.New("token expired") }

	findings := conform.Check(t.Context(), newStub(broken), conform.Fixture{Docs: 4}, nil).Findings
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one failure the others all derive from", findings)
	}
	if findings[0].Check != conform.CheckStream || !strings.Contains(findings[0].Detail, "token expired") {
		t.Errorf("finding = %+v, want the stream failure carrying the connector's error", findings[0])
	}
}

func TestADeclaredCountThatDisagreesIsAFailure(t *testing.T) {
	findings := conform.Check(t.Context(), newStub(conformant), conform.Fixture{Docs: 5}, nil).Findings
	if len(findings) != 1 || findings[0].Check != conform.CheckStream {
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

	findings := conform.Check(t.Context(), newStub(single), conform.Fixture{Docs: 1}, nil).Findings
	if len(findings) != 1 || findings[0].Check != conform.CheckResumable {
		t.Fatalf("findings = %+v, want the resume check to report that it could not run", findings)
	}
}

type scripted struct {
	changes iter.Seq2[lore.Batch, error]
}

func (scripted) Name() string { return "stub" }

func (s scripted) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return s.changes
}

func TestAnErrorHasToEndTheStream(t *testing.T) {
	full, _ := conformant(nil)
	tests := map[string]struct {
		changes iter.Seq2[lore.Batch, error]
		detail  string
	}{
		"a connector that keeps yielding after an error": {
			changes: func(yield func(lore.Batch, error) bool) {
				if !yield(full[0], nil) {
					return
				}
				if !yield(lore.Batch{}, errors.New("page 2 timed out")) {
					return
				}
				for _, batch := range []lore.Batch{full[1], full[0], full[1]} {
					if !yield(batch, nil) {
						return
					}
				}
			},
			detail: "full stream: the stream yielded a batch carrying 2 documents after an error",
		},
		"a connector that yields a batch beside the error": {
			changes: func(yield func(lore.Batch, error) bool) {
				if !yield(full[0], nil) {
					return
				}
				yield(full[1], errors.New("page 2 timed out"))
			},
			detail: "full stream: an error arrived in the same yield as a batch carrying 2 documents and 1 cursor keys",
		},
		"a connector that yields a cursor beside the error": {
			changes: func(yield func(lore.Batch, error) bool) {
				if !yield(full[0], nil) {
					return
				}
				yield(lore.Batch{Cursor: lore.Cursor{"after": "4"}}, errors.New("page 2 timed out"))
			},
			detail: "full stream: an error arrived in the same yield as a batch carrying 0 documents and 1 cursor keys",
		},
		"a connector that yields a second error after the first": {
			changes: func(yield func(lore.Batch, error) bool) {
				if !yield(full[0], nil) {
					return
				}
				if !yield(lore.Batch{}, errors.New("page 2 timed out")) {
					return
				}
				yield(lore.Batch{}, errors.New("page 3 timed out too"))
			},
			detail: "full stream: the stream yielded another error after the first",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			result := conform.Check(t.Context(), func() lore.Connector { return scripted{tt.changes} },
				conform.Fixture{Docs: 4}, nil)
			if !slices.Contains(result.Ran, conform.CheckStreamError) {
				t.Fatalf("ran = %v, want the error check among them", result.Ran)
			}

			reported := 0
			for _, f := range result.Findings {
				if f.Check == conform.CheckStreamError && strings.Contains(f.Detail, tt.detail) {
					reported++
				}
			}
			if reported != 1 {
				t.Errorf("findings %v report %q containing %q %d times, want once; details: %+v",
					checkNames(result.Findings), conform.CheckStreamError, tt.detail, reported, result.Findings)
			}
		})
	}
}

func TestAStreamThatNeverFailsStillRunsTheErrorCheck(t *testing.T) {
	result := conform.Check(t.Context(), newStub(conformant), conform.Fixture{Docs: 4}, nil)
	if !slices.Contains(result.Ran, conform.CheckStreamError) {
		t.Errorf("ran = %v, want the error check among them", result.Ran)
	}
	if len(result.Skipped) != 0 {
		t.Errorf("a conformant connector reduced the run: %+v", result.Skipped)
	}
}

func TestTheErrorCheckWatchesEveryStreamTheSuiteDrives(t *testing.T) {
	full, _ := conformant(nil)
	runs := 0
	newConnector := func() lore.Connector {
		runs++
		if runs == 1 {
			return stub{name: "stub", stream: conformant}
		}
		return scripted{changes: func(yield func(lore.Batch, error) bool) {
			if !yield(full[0], nil) {
				return
			}
			if !yield(lore.Batch{}, errors.New("page 2 timed out")) {
				return
			}
			yield(full[1], nil)
		}}
	}

	result := conform.Check(t.Context(), newConnector, conform.Fixture{Docs: 4}, nil)
	for _, f := range result.Findings {
		if f.Check == conform.CheckStreamError &&
			strings.Contains(f.Detail, "second full stream: the stream yielded a batch carrying 2 documents after an error") {
			return
		}
	}
	t.Errorf("the idempotency re-run went unwatched: %+v", result.Findings)
}

func TestASourceThatDeclinesIsStillHeldToTheErrorContract(t *testing.T) {
	refusing := func() lore.Connector {
		return scripted{changes: func(yield func(lore.Batch, error) bool) {
			yield(lore.Batch{Cursor: lore.Cursor{"after": "0"}}, errors.New("no team is configured"))
		}}
	}
	unconfigured := func(error) bool { return true }

	result := conform.Check(t.Context(), refusing, conform.Fixture{}, unconfigured)
	if !slices.Contains(result.Ran, conform.CheckStreamError) {
		t.Fatalf("ran = %v, skipped = %+v: a violation observed beside the refusal was thrown away",
			result.Ran, result.Skipped)
	}
	for _, f := range result.Findings {
		if f.Check == conform.CheckStreamError && strings.Contains(f.Detail, "0 documents and 1 cursor keys") {
			return
		}
	}
	t.Errorf("findings %+v do not report the cursor the refusal carried", result.Findings)
}
