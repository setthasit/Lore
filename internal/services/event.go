package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/setthasit/Lore/internal/connectors/embedder"
	"github.com/setthasit/Lore/internal/entities"
)

const (
	eventTopHits       = 5
	eventGapCandidates = 3
)

var eventDateLayouts = []string{time.DateOnly, time.RFC3339}

type eventSource interface {
	searchSource
	documentSource
}

type eventOptions struct {
	Window time.Duration
	TopK   int
}

type resolvedEvent struct {
	Window *entities.TimeWindow
	Gap    string
}

// An event that cannot be dated is a Gap, never an error: the query proceeds unwindowed.
func resolveEvent(
	ctx context.Context,
	s eventSource,
	emb embedder.Embedder,
	around string,
	opts eventOptions,
) (resolvedEvent, error) {
	event := strings.TrimSpace(around)
	if event == "" {
		return resolvedEvent{}, nil
	}

	if at, dated := parseEventDate(event); dated {
		derivation := fmt.Sprintf("date %s ± %s", formatEventInstant(at), formatHalfWidth(opts.Window))

		return resolvedEvent{Window: windowAround(at, opts.Window, derivation, "")}, nil
	}

	candidates, err := eventCandidates(ctx, s, emb, event, opts.TopK)
	if err != nil {
		return resolvedEvent{}, err
	}
	if len(candidates) == 0 {
		return resolvedEvent{Gap: fmt.Sprintf(
			"could not resolve event %q to a time — nothing in the index matched the event text", event)}, nil
	}

	anchor, span := earliestAndSpan(candidates)
	if span > fullWidth(opts.Window) {
		return resolvedEvent{Gap: scatteredGap(event, candidates)}, nil
	}

	derivation := fmt.Sprintf("event %q dated %s via %s",
		event, anchor.Meta.CreatedAt.Format(time.DateOnly), anchor.Meta.ID)

	return resolvedEvent{Window: windowAround(anchor.Meta.CreatedAt, opts.Window, derivation, anchor.Meta.ID)}, nil
}

func eventCandidates(
	ctx context.Context,
	s eventSource,
	emb embedder.Embedder,
	event string,
	topK int,
) ([]seedHit, error) {
	fused, err := hybridSearch(ctx, s, emb, event, entities.Filters{}, topK)
	if err != nil {
		return nil, err
	}
	seeds, err := liftDocuments(ctx, s, fused)
	if err != nil {
		return nil, err
	}

	return seeds[:min(len(seeds), eventTopHits)], nil
}

func parseEventDate(s string) (time.Time, bool) {
	for _, layout := range eventDateLayouts {
		if at, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return at.UTC(), true
		}
	}

	return time.Time{}, false
}

func windowAround(at time.Time, halfWidth time.Duration, derivation string, anchor entities.DocID) *entities.TimeWindow {
	return &entities.TimeWindow{
		From:       at.Add(-halfWidth),
		To:         at.Add(halfWidth),
		Derivation: derivation,
		AnchoredBy: anchor,
	}
}

func fullWidth(halfWidth time.Duration) time.Duration {
	return halfWidth * 2
}

func formatHalfWidth(d time.Duration) string {
	const day = 24 * time.Hour

	if d%day == 0 {
		return fmt.Sprintf("%dd", d/day)
	}

	return d.String()
}

// A midnight instant came from a date-only input; anything else would misstate
// the window centre by up to a day.
func formatEventInstant(at time.Time) string {
	if at.Equal(at.Truncate(24 * time.Hour)) {
		return at.Format(time.DateOnly)
	}

	return at.Format(time.RFC3339)
}

func earliestAndSpan(candidates []seedHit) (seedHit, time.Duration) {
	anchor := candidates[0]
	latest := candidates[0].Meta.CreatedAt
	for _, c := range candidates[1:] {
		if c.Meta.CreatedAt.Before(anchor.Meta.CreatedAt) {
			anchor = c
		}
		if c.Meta.CreatedAt.After(latest) {
			latest = c.Meta.CreatedAt
		}
	}

	return anchor, latest.Sub(anchor.Meta.CreatedAt)
}

func scatteredGap(event string, candidates []seedHit) string {
	listed := candidates[:min(len(candidates), eventGapCandidates)]
	described := make([]string, 0, len(listed))
	for _, c := range listed {
		described = append(described, fmt.Sprintf("%s (%s) %s",
			c.Meta.Title, c.Meta.CreatedAt.Format(time.DateOnly), c.Meta.URL))
	}

	return fmt.Sprintf("could not resolve event %q to a time — candidates: %s",
		event, strings.Join(described, "; "))
}
