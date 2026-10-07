package mcp

import (
	"fmt"
	"strings"
	"time"

	"github.com/setthasit/Lore/internal/entities"
)

const indexOverviewLine = "Lore indexes this project's design pages, decisions, tickets, pull requests and commits."

const serverRoutingInstructions = `Use find_decision first for why, design and decision questions, before reading local files.
Use why for a named file and line, trace for one document in depth, impact_of for what followed a decision, history_of for how one file evolved.
If a result is empty or holds fewer than 3 documents, call sync_status, then sync_now if a source is stale or never synced.`

const unavailableIndexLine = "Index state is unavailable. Call sync_status to check it."

func serverInstructions(stats entities.IndexStats, now time.Time) string {
	lines := []string{indexOverviewLine}
	var sources []string
	for i, source := range stats.Sources {
		if !source.Configured && source.Documents == 0 {
			continue
		}
		sources = append(sources, sourceInstructions(source, i+1, now))
	}
	if len(sources) > 0 {
		lines = append(lines, "Indexed sources: "+strings.Join(sources, "; ")+".")
	}
	if stats.Documents == 0 {
		lines = append(lines, "The index is empty. Call sync_now before asking anything.")
	}
	lines = append(lines, serverRoutingInstructions)
	return strings.Join(lines, "\n")
}

func sourceInstructions(source entities.SourceState, position int, now time.Time) string {
	state := sourceInstructionState(source, now)
	if source.Documents > 0 {
		unit := "documents"
		if source.Documents == 1 {
			unit = "document"
		}
		state = fmt.Sprintf("%d %s, %s", source.Documents, unit, state)
	}
	return fmt.Sprintf("%s (%s)", instructionSourceLabel(source.ID, position), state)
}

func sourceInstructionState(source entities.SourceState, now time.Time) string {
	if source.LastCheckpoint.IsZero() {
		if source.Configured {
			return "configured, never synced"
		}
		return "never synced"
	}
	minutes := max(0, int64(now.Sub(source.LastCheckpoint)/time.Minute))
	if minutes == 0 {
		return "synced under a minute ago"
	}
	return fmt.Sprintf("synced %d minutes ago", minutes)
}

func instructionSourceLabel(id string, position int) string {
	const maxSourceLabelLength = 64
	fallback := fmt.Sprintf("source %d", position)
	if len(id) == 0 || len(id) > maxSourceLabelLength {
		return fallback
	}
	for _, character := range id {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case character == '.', character == '_', character == '-':
		default:
			return fallback
		}
	}
	return id
}
