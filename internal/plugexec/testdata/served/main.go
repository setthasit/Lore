package main

import (
	"context"
	"fmt"
	"iter"
	"os"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/stdio"
)

func main() {
	if err := stdio.Serve(plugin{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type plugin struct{}

func (plugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       "served",
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "release notes, newest last (read-only)",
	}
}

func (plugin) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	return notes{cfg: c}, nil
}

const noteType lore.DocType = "note"

type note struct {
	external  string
	title     string
	body      string
	createdAt time.Time
	updatedAt time.Time
}

var releaseNotes = []note{
	{
		external:  "v0.1",
		title:     "v0.1",
		body:      "The first tagged build.",
		createdAt: time.Date(2026, 3, 2, 9, 15, 0, 0, time.UTC),
		updatedAt: time.Date(2026, 3, 4, 11, 0, 0, 0, time.UTC),
	},
	{
		external:  "v0.2",
		title:     "v0.2",
		body:      "Resumable syncs.",
		createdAt: time.Date(2026, 4, 17, 8, 5, 0, 0, time.UTC),
		updatedAt: time.Date(2026, 4, 17, 8, 5, 0, 0, time.UTC),
	},
	{
		external:  "v0.3",
		title:     "v0.3",
		body:      "Citations carry a URL.",
		createdAt: time.Date(2026, 5, 28, 16, 40, 0, 0, time.UTC),
		updatedAt: time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC),
	},
}

const lastNote = "last_note"

type notes struct {
	cfg lore.SourceConfig
}

func (n notes) Name() string { return n.cfg.Instance }

func (n notes) Changes(_ context.Context, cursor lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) {
		for _, release := range releaseNotes[resumeAt(cursor):] {
			batch := lore.Batch{
				Docs:   []lore.Document{n.document(release)},
				Cursor: lore.Cursor{lastNote: release.external},
			}
			if !yield(batch, nil) {
				return
			}
		}
	}
}

func resumeAt(cursor lore.Cursor) int {
	for i, release := range releaseNotes {
		if release.external == cursor[lastNote] {
			return i + 1
		}
	}
	return 0
}

func (n notes) document(release note) lore.Document {
	return lore.Document{
		ID:        n.cfg.DocID(noteType, release.external),
		Source:    n.cfg.Instance,
		Type:      noteType,
		Title:     release.title,
		Body:      release.body,
		Author:    "release-bot",
		URL:       "https://notes.example.test/releases/" + release.external,
		CreatedAt: release.createdAt,
		UpdatedAt: release.updatedAt,
	}
}
