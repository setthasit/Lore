package notion

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"lore/internal/connectors/refscan"
	"lore/internal/entities"
)

const (
	sourceName = "notion"

	defaultBatchSize = 50

	// maxAncestorDepth bounds the upward walk; Notion exposes only a page's immediate parent.
	maxAncestorDepth = 32

	cursorLastEditedKey = "last_edited_at"
	cursorDocKey        = "doc_id"
)

var _ entities.Connector = (*Connector)(nil)

type Connector struct {
	client    *client
	rootPages []string
	batchSize int
}

type Option func(*Connector)

func WithHTTPClient(h *http.Client) Option {
	return func(c *Connector) {
		if h != nil {
			c.client.http = h
		}
	}
}

func withBatchSize(n int) Option {
	return func(c *Connector) {
		if n > 0 {
			c.batchSize = n
		}
	}
}

func withMaxAttempts(n int) Option {
	return func(c *Connector) {
		if n > 0 {
			c.client.maxAttempts = n
		}
	}
}

func withBackoff(base time.Duration) Option {
	return func(c *Connector) { c.client.baseBackoff = base }
}

// NewConnector scopes the sync to rootPages and their descendants, each entry a page
// id or an exact page title; empty rootPages means every page the token can read.
func NewConnector(token string, rootPages []string, baseURL string, opts ...Option) *Connector {
	c := &Connector{
		client:    newClient(token, baseURL),
		rootPages: slices.Clone(rootPages),
		batchSize: defaultBatchSize,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Connector) Name() string { return sourceName }

func (c *Connector) Changes(ctx context.Context, cursor entities.Cursor) iter.Seq2[entities.Batch, error] {
	return func(yield func(entities.Batch, error) bool) {
		state := cloneCursor(cursor)
		from, err := readCursor(state)
		if err != nil {
			yield(entities.Batch{}, err)
			return
		}
		sc, err := c.resolveScope(ctx)
		if err != nil {
			yield(entities.Batch{}, err)
			return
		}

		docs := make([]entities.Document, 0, c.batchSize)
		pos := from
		stopped := false
		var buildErr error

		walkErr := c.client.eachPage(ctx, "", func(p *page) bool {
			// Notion orders search by last_edited_time alone, so a batch never closes mid-timestamp.
			if len(docs) >= c.batchSize && p.LastEditedTime.After(pos.lastEditedAt) {
				if !yield(entities.Batch{Docs: docs, Cursor: cloneCursor(state)}, nil) {
					stopped = true
					return false
				}
				docs = make([]entities.Document, 0, c.batchSize)
			}

			key := pageKey{lastEditedAt: p.LastEditedTime, docID: docID(p.ID)}
			if p.trashed() || !key.after(from) {
				return true
			}
			inScope, err := c.inScope(ctx, sc, p)
			if err != nil {
				buildErr = err
				return false
			}
			if !inScope {
				return true
			}
			doc, err := c.document(ctx, p)
			if err != nil {
				buildErr = err
				return false
			}
			docs = append(docs, doc)
			if key.after(pos) {
				pos = key
				writeCursor(state, pos)
			}
			return true
		})

		switch {
		case stopped:
			return
		case buildErr != nil:
			yield(entities.Batch{}, fmt.Errorf("notion: %w", buildErr))
			return
		case walkErr != nil:
			yield(entities.Batch{}, fmt.Errorf("notion: %w", walkErr))
			return
		}
		if len(docs) > 0 {
			yield(entities.Batch{Docs: docs, Cursor: cloneCursor(state)}, nil)
		}
	}
}

func (c *Connector) document(ctx context.Context, p *page) (entities.Document, error) {
	blocks, err := c.client.blockTree(ctx, p.ID, 0)
	if err != nil {
		return entities.Document{}, fmt.Errorf("page %s: %w", p.ID, err)
	}
	title, body := p.title(), flatten(blocks)
	created, updated := p.CreatedTime, p.LastEditedTime
	if created.IsZero() {
		created = updated
	}
	if updated.IsZero() {
		updated = created
	}

	var refs refscan.Set
	text := title + "\n" + body
	refs.AddTicketKeys(text)
	refs.AddURLs(text)
	refs.AddCommitSHAs(text)
	refs.AddFilePaths(text)

	return entities.Document{
		ID:        docID(p.ID),
		Source:    sourceName,
		Type:      entities.DocTypePage,
		Title:     title,
		Body:      body,
		URL:       p.URL,
		CreatedAt: created,
		UpdatedAt: updated,
		Refs:      refs.Refs(),
	}, nil
}

func docID(pageID string) entities.DocID {
	return entities.NewDocID(sourceName, entities.DocTypePage, pageID)
}

type scope struct {
	roots map[string]bool
	memo  map[string]bool
}

func (s *scope) record(ids []string, verdict bool) bool {
	for _, id := range ids {
		s.memo[id] = verdict
	}
	return verdict
}

func (c *Connector) resolveScope(ctx context.Context) (*scope, error) {
	sc := &scope{
		roots: make(map[string]bool, len(c.rootPages)),
		memo:  make(map[string]bool),
	}
	for _, entry := range c.rootPages {
		id, err := c.resolveRoot(ctx, entry)
		if err != nil {
			return nil, err
		}
		sc.roots[id] = true
	}
	return sc, nil
}

func (c *Connector) resolveRoot(ctx context.Context, entry string) (string, error) {
	title := strings.TrimSpace(entry)
	if title == "" {
		return "", fmt.Errorf("notion: blank root page entry")
	}
	if id := normalizeID(title); isPageID(id) {
		if err := c.confirmRoot(ctx, entry, id); err != nil {
			return "", err
		}
		return id, nil
	}

	var matches []string
	err := c.client.eachPage(ctx, title, func(p *page) bool {
		if !p.trashed() && p.title() == title {
			matches = append(matches, p.ID)
		}
		return len(matches) < 2
	})
	if err != nil {
		return "", fmt.Errorf("notion: resolve root page %q: %w", entry, err)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("notion: root page %q matches no page title", entry)
	case 1:
		return normalizeID(matches[0]), nil
	}
	return "", fmt.Errorf("notion: root page %q matches the live pages %s and %s: configure it by id",
		entry, matches[0], matches[1])
}

func (c *Connector) confirmRoot(ctx context.Context, entry, id string) error {
	p, err := c.client.pageByID(ctx, id)
	if err != nil {
		return fmt.Errorf("notion: root page %q reads back as no page: %w", entry, err)
	}
	if p.trashed() {
		return fmt.Errorf("notion: root page %q is in the trash", entry)
	}
	return nil
}

func (c *Connector) inScope(ctx context.Context, sc *scope, p *page) (bool, error) {
	if len(sc.roots) == 0 {
		return true, nil
	}
	id := normalizeID(p.ID)
	if sc.roots[id] {
		return true, nil
	}

	visited := []string{id}
	ref := p.Parent
	for range maxAncestorDepth {
		parentID := ref.id()
		if parentID == "" {
			return sc.record(visited, false), nil
		}
		normalized := normalizeID(parentID)
		if sc.roots[normalized] {
			return sc.record(visited, true), nil
		}
		if verdict, ok := sc.memo[normalized]; ok {
			return sc.record(visited, verdict), nil
		}
		visited = append(visited, normalized)

		var err error
		if ref.Type == parentBlock {
			ref, err = c.client.blockParent(ctx, parentID)
		} else {
			ref, err = c.client.pageParent(ctx, parentID)
		}
		if err != nil {
			return false, err
		}
	}
	return false, nil
}

// Notion writes page ids dashed; configuration may not.
func normalizeID(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}

func isPageID(normalized string) bool {
	if len(normalized) != 32 {
		return false
	}
	for _, r := range normalized {
		if !isHexDigit(r) {
			return false
		}
	}
	return true
}

func isHexDigit(r rune) bool { return '0' <= r && r <= '9' || 'a' <= r && r <= 'f' }

type pageKey struct {
	lastEditedAt time.Time
	docID        entities.DocID
}

func (k pageKey) after(o pageKey) bool {
	if c := k.lastEditedAt.Compare(o.lastEditedAt); c != 0 {
		return c > 0
	}
	return strings.Compare(string(k.docID), string(o.docID)) > 0
}

func readCursor(c entities.Cursor) (pageKey, error) {
	raw := c[cursorLastEditedKey]
	if raw == "" {
		return pageKey{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return pageKey{}, fmt.Errorf("notion: parse cursor watermark %q: %w", raw, err)
	}
	return pageKey{lastEditedAt: at, docID: entities.DocID(c[cursorDocKey])}, nil
}

// Notion edit times carry milliseconds, so the watermark keeps them: truncating
// would replay every page sharing the second.
func writeCursor(c entities.Cursor, k pageKey) {
	c[cursorLastEditedKey] = k.lastEditedAt.UTC().Format(time.RFC3339Nano)
	c[cursorDocKey] = string(k.docID)
}

// A yielded batch owns its own map: the caller persists it while the iterator advances.
func cloneCursor(c entities.Cursor) entities.Cursor {
	if len(c) == 0 {
		return entities.Cursor{}
	}
	return maps.Clone(c)
}
