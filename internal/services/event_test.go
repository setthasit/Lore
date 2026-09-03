package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	mock_embedder "github.com/setthasit/Lore/internal/mocks/embedder"
	mock_repositories "github.com/setthasit/Lore/internal/mocks/repositories"
)

const (
	eventText      = "incident X"
	eventTopK      = 8
	eventHalfWidth = 30 * 24 * time.Hour
)

var eventVector = []float32{0.75, 0.25}

var eventEpoch = time.Date(2025, time.March, 12, 0, 0, 0, 0, time.UTC)

type eventMocks struct {
	store *mock_repositories.MockIndexStore
	emb   *mock_embedder.MockEmbedder
}

func newEventMocks(t *testing.T) eventMocks {
	t.Helper()

	ctrl := gomock.NewController(t)

	return eventMocks{
		store: mock_repositories.NewMockIndexStore(ctrl),
		emb:   mock_embedder.NewMockEmbedder(ctrl),
	}
}

func (m eventMocks) expectRetrieval(metas ...entities.DocumentMeta) {
	hits := make([]entities.ChunkHit, 0, len(metas))
	ids := make([]entities.DocID, 0, len(metas))
	for _, meta := range metas {
		hits = append(hits, hit(string(meta.ID), 0))
		ids = append(ids, meta.ID)
	}

	m.emb.EXPECT().Embed(gomock.Any(), []string{eventText}).Return([][]float32{eventVector}, nil)
	m.store.EXPECT().
		SearchLexical(gomock.Any(), eventText, gomock.Eq(entities.Filters{}), eventTopK).
		Return(hits, nil)
	m.store.EXPECT().
		SearchVector(gomock.Any(), eventVector, gomock.Eq(entities.Filters{}), eventTopK).
		Return(nil, nil)
	if len(metas) == 0 {
		return
	}
	m.store.EXPECT().DocumentsByID(gomock.Any(), ids).Return(metas, nil)
}

func eventMeta(id string, at time.Time) entities.DocumentMeta {
	return entities.DocumentMeta{
		ID:        entities.DocID(id),
		Source:    "github",
		Type:      entities.DocTypeIssue,
		Title:     "incident report " + id,
		URL:       "https://example.test/" + id,
		CreatedAt: at,
	}
}

func eventDays(n int) time.Time {
	return eventEpoch.AddDate(0, 0, n)
}

func resolveEventIn(t *testing.T, m eventMocks, around string, halfWidth time.Duration) resolvedEvent {
	t.Helper()

	got, err := resolveEvent(context.Background(), m.store, m.emb, around,
		eventOptions{Window: halfWidth, TopK: eventTopK})
	if err != nil {
		t.Fatalf("resolveEvent: %v", err)
	}

	return got
}

func assertWindow(t *testing.T, got resolvedEvent, want entities.TimeWindow) {
	t.Helper()

	if got.Gap != "" {
		t.Errorf("gap = %q, want none", got.Gap)
	}
	if got.Window == nil {
		t.Fatalf("window = nil, want %+v", want)
	}
	if !got.Window.From.Equal(want.From) || !got.Window.To.Equal(want.To) {
		t.Errorf("window = %s..%s, want %s..%s",
			got.Window.From, got.Window.To, want.From, want.To)
	}
	if got.Window.Derivation != want.Derivation {
		t.Errorf("derivation = %q, want %q", got.Window.Derivation, want.Derivation)
	}
	if got.Window.AnchoredBy != want.AnchoredBy {
		t.Errorf("anchored by %q, want %q", got.Window.AnchoredBy, want.AnchoredBy)
	}
}

func assertUnresolved(t *testing.T, got resolvedEvent, wantGap string) {
	t.Helper()

	if got.Window != nil {
		t.Errorf("window = %+v, want none", got.Window)
	}
	if got.Gap != wantGap {
		t.Errorf("gap = %q, want %q", got.Gap, wantGap)
	}
}

func TestResolveEventDatesAnISODateWithoutRetrieval(t *testing.T) {
	t.Parallel()

	for name, around := range map[string]string{
		"date only": "  2025-03-12  ",
		"rfc3339":   "2025-03-12T00:00:00Z",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := newEventMocks(t)

			assertWindow(t, resolveEventIn(t, m, around, eventHalfWidth), entities.TimeWindow{
				From:       eventDays(-30),
				To:         eventDays(30),
				Derivation: "date 2025-03-12 ± 30d",
			})
		})
	}
}

func TestResolveEventDatesADatetimeToTheInstant(t *testing.T) {
	t.Parallel()

	at := eventEpoch.Add(14*time.Hour + 30*time.Minute)

	assertWindow(t, resolveEventIn(t, newEventMocks(t), "2025-03-12T21:30:00+07:00", eventHalfWidth),
		entities.TimeWindow{
			From:       at.Add(-eventHalfWidth),
			To:         at.Add(eventHalfWidth),
			Derivation: "date 2025-03-12T14:30:00Z ± 30d",
		})
}

func TestResolveEventReportsWholeDayWindowsInDays(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		halfWidth time.Duration
		want      entities.TimeWindow
	}{
		"whole days": {
			halfWidth: eventHalfWidth,
			want: entities.TimeWindow{
				From:       eventEpoch.Add(-eventHalfWidth),
				To:         eventEpoch.Add(eventHalfWidth),
				Derivation: "date 2025-03-12 ± 30d",
			},
		},
		"partial day": {
			halfWidth: 36 * time.Hour,
			want: entities.TimeWindow{
				From:       eventEpoch.Add(-36 * time.Hour),
				To:         eventEpoch.Add(36 * time.Hour),
				Derivation: "date 2025-03-12 ± 36h0m0s",
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assertWindow(t, resolveEventIn(t, newEventMocks(t), "2025-03-12", tc.halfWidth), tc.want)
		})
	}
}

func TestResolveEventAnchorsAgreeingHitsOnTheEarliest(t *testing.T) {
	t.Parallel()

	m := newEventMocks(t)
	m.expectRetrieval(
		eventMeta("top", eventDays(10)),
		eventMeta("early", eventEpoch),
		eventMeta("mid", eventDays(5)),
	)

	assertWindow(t, resolveEventIn(t, m, eventText, eventHalfWidth), entities.TimeWindow{
		From:       eventDays(-30),
		To:         eventDays(30),
		Derivation: `event "incident X" dated 2025-03-12 via early`,
		AnchoredBy: "early",
	})
}

func TestResolveEventAcceptsASingleHit(t *testing.T) {
	t.Parallel()

	m := newEventMocks(t)
	m.expectRetrieval(eventMeta("solo", eventEpoch))

	assertWindow(t, resolveEventIn(t, m, eventText, eventHalfWidth), entities.TimeWindow{
		From:       eventDays(-30),
		To:         eventDays(30),
		Derivation: `event "incident X" dated 2025-03-12 via solo`,
		AnchoredBy: "solo",
	})
}

func TestResolveEventAcceptsASpanOfExactlyTwoWindows(t *testing.T) {
	t.Parallel()

	m := newEventMocks(t)
	m.expectRetrieval(
		eventMeta("early", eventEpoch),
		eventMeta("late", eventEpoch.Add(2*eventHalfWidth)),
	)

	assertWindow(t, resolveEventIn(t, m, eventText, eventHalfWidth), entities.TimeWindow{
		From:       eventDays(-30),
		To:         eventDays(30),
		Derivation: `event "incident X" dated 2025-03-12 via early`,
		AnchoredBy: "early",
	})
}

func TestResolveEventRejectsASpanJustBeyondTwoWindows(t *testing.T) {
	t.Parallel()

	late := eventMeta("late", eventEpoch.Add(2*eventHalfWidth+time.Microsecond))

	m := newEventMocks(t)
	m.expectRetrieval(eventMeta("early", eventEpoch), late)

	assertUnresolved(t, resolveEventIn(t, m, eventText, eventHalfWidth),
		`could not resolve event "incident X" to a time — candidates: `+
			"incident report early (2025-03-12) https://example.test/early; "+
			"incident report late (2025-05-11) https://example.test/late")
}

func TestResolveEventIgnoresHitsBeyondTheCandidateCap(t *testing.T) {
	t.Parallel()

	m := newEventMocks(t)
	m.expectRetrieval(
		eventMeta("c0", eventDays(8)),
		eventMeta("c1", eventDays(6)),
		eventMeta("c2", eventEpoch),
		eventMeta("c3", eventDays(4)),
		eventMeta("c4", eventDays(2)),
		eventMeta("c5", eventEpoch.AddDate(2, 0, 0)),
	)

	assertWindow(t, resolveEventIn(t, m, eventText, eventHalfWidth), entities.TimeWindow{
		From:       eventDays(-30),
		To:         eventDays(30),
		Derivation: `event "incident X" dated 2025-03-12 via c2`,
		AnchoredBy: "c2",
	})
}

func TestResolveEventReportsScatteredHitsAsAGap(t *testing.T) {
	t.Parallel()

	m := newEventMocks(t)
	m.expectRetrieval(
		eventMeta("d1", eventEpoch),
		eventMeta("d2", time.Date(2025, time.June, 10, 0, 0, 0, 0, time.UTC)),
		eventMeta("d3", time.Date(2024, time.November, 2, 0, 0, 0, 0, time.UTC)),
		eventMeta("d4", time.Date(2025, time.September, 1, 0, 0, 0, 0, time.UTC)),
	)

	assertUnresolved(t, resolveEventIn(t, m, eventText, eventHalfWidth),
		`could not resolve event "incident X" to a time — candidates: `+
			"incident report d1 (2025-03-12) https://example.test/d1; "+
			"incident report d2 (2025-06-10) https://example.test/d2; "+
			"incident report d3 (2024-11-02) https://example.test/d3")
}

func TestResolveEventReportsAnUnmatchedEventAsAGap(t *testing.T) {
	t.Parallel()

	m := newEventMocks(t)
	m.expectRetrieval()

	assertUnresolved(t, resolveEventIn(t, m, eventText, eventHalfWidth),
		`could not resolve event "incident X" to a time — nothing in the index matched the event text`)
}

func TestResolveEventNeverCitesACandidateWithoutAURL(t *testing.T) {
	t.Parallel()

	urlless := eventMeta("urlless", eventDays(-20))
	urlless.URL = ""

	t.Run("anchor", func(t *testing.T) {
		t.Parallel()

		m := newEventMocks(t)
		m.expectRetrieval(urlless, eventMeta("late", eventDays(5)), eventMeta("early", eventEpoch))

		assertWindow(t, resolveEventIn(t, m, eventText, eventHalfWidth), entities.TimeWindow{
			From:       eventDays(-30),
			To:         eventDays(30),
			Derivation: `event "incident X" dated 2025-03-12 via early`,
			AnchoredBy: "early",
		})
	})

	t.Run("gap", func(t *testing.T) {
		t.Parallel()

		m := newEventMocks(t)
		m.expectRetrieval(
			urlless,
			eventMeta("d1", eventEpoch),
			eventMeta("d2", time.Date(2025, time.June, 10, 0, 0, 0, 0, time.UTC)),
			eventMeta("d3", time.Date(2024, time.November, 2, 0, 0, 0, 0, time.UTC)),
			eventMeta("d4", time.Date(2025, time.September, 1, 0, 0, 0, 0, time.UTC)),
		)

		assertUnresolved(t, resolveEventIn(t, m, eventText, eventHalfWidth),
			`could not resolve event "incident X" to a time — candidates: `+
				"incident report d1 (2025-03-12) https://example.test/d1; "+
				"incident report d2 (2025-06-10) https://example.test/d2; "+
				"incident report d3 (2024-11-02) https://example.test/d3")
	})
}

func TestResolveEventWithoutAnEventTouchesNothing(t *testing.T) {
	t.Parallel()

	m := newEventMocks(t)

	got := resolveEventIn(t, m, "  \t\n ", eventHalfWidth)
	if got.Window != nil || got.Gap != "" {
		t.Errorf("resolved = %+v, want the zero result", got)
	}
}

func TestResolveEventPropagatesRetrievalFailures(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		expect func(m eventMocks)
		cause  error
	}{
		"embedder": {
			expect: func(m eventMocks) {
				m.emb.EXPECT().Embed(gomock.Any(), []string{eventText}).Return(nil, errRetrieveEmbed)
			},
			cause: errRetrieveEmbed,
		},
		"lexical search": {
			expect: func(m eventMocks) {
				m.emb.EXPECT().Embed(gomock.Any(), []string{eventText}).
					Return([][]float32{eventVector}, nil)
				m.store.EXPECT().SearchLexical(gomock.Any(), eventText, gomock.Any(), eventTopK).
					Return(nil, errRetrieveStore)
			},
			cause: errRetrieveStore,
		},
		"document metadata": {
			expect: func(m eventMocks) {
				m.emb.EXPECT().Embed(gomock.Any(), []string{eventText}).
					Return([][]float32{eventVector}, nil)
				m.store.EXPECT().SearchLexical(gomock.Any(), eventText, gomock.Any(), eventTopK).
					Return([]entities.ChunkHit{hit("d1", 0)}, nil)
				m.store.EXPECT().SearchVector(gomock.Any(), eventVector, gomock.Any(), eventTopK).
					Return(nil, nil)
				m.store.EXPECT().DocumentsByID(gomock.Any(), []entities.DocID{"d1"}).
					Return(nil, errRetrieveStore)
			},
			cause: errRetrieveStore,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := newEventMocks(t)
			tc.expect(m)

			got, err := resolveEvent(context.Background(), m.store, m.emb, eventText,
				eventOptions{Window: eventHalfWidth, TopK: eventTopK})
			if !errors.Is(err, tc.cause) {
				t.Fatalf("err = %v, want %v wrapped", err, tc.cause)
			}
			if got.Window != nil || got.Gap != "" {
				t.Errorf("resolved = %+v, want the zero result", got)
			}
		})
	}
}
