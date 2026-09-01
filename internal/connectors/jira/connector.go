package jira

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
	sourceName = "jira"

	// defaultBatchSize closes a batch on the next unit boundary, so it may overshoot.
	defaultBatchSize = 50

	cursorUpdatedKey = "updated_at"
	cursorDocKey     = "doc_id"

	jqlOrder      = "ORDER BY updated ASC"
	jqlTimeLayout = "2006-01-02 15:04"
	jqlSlack      = 24 * time.Hour
)

var _ entities.Connector = (*Connector)(nil)

type Connector struct {
	client    *client
	baseURL   string
	projects  []string
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

// NewConnector builds a connector for a Jira Cloud site ("https://acme.atlassian.net").
// An empty projects list ingests every project the credentials can browse.
func NewConnector(baseURL, email, token string, projects []string, opts ...Option) *Connector {
	root := strings.TrimSuffix(baseURL, "/")
	c := &Connector{
		client:    newClient(root, email, token),
		baseURL:   root,
		projects:  slices.Clone(projects),
		batchSize: defaultBatchSize,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Connector) Name() string { return sourceName }

// Changes streams each issue with its comments as one indivisible unit, oldest-first.
func (c *Connector) Changes(ctx context.Context, cursor entities.Cursor) iter.Seq2[entities.Batch, error] {
	return func(yield func(entities.Batch, error) bool) {
		if err := c.checkProjectKeys(); err != nil {
			yield(entities.Batch{}, err)
			return
		}
		state := cloneCursor(cursor)
		from, err := readCursor(state)
		if err != nil {
			yield(entities.Batch{}, err)
			return
		}
		units, err := c.units(ctx, from)
		if err != nil {
			yield(entities.Batch{}, fmt.Errorf("jira: %w", err))
			return
		}

		docs := make([]entities.Document, 0, c.batchSize)
		for i := range units {
			u := &units[i]
			docs = append(docs, u.docs...)
			writeCursor(state, u.key)
			if len(docs) < c.batchSize {
				continue
			}
			if !yield(entities.Batch{Docs: docs, Cursor: cloneCursor(state)}, nil) {
				return
			}
			docs = make([]entities.Document, 0, c.batchSize)
		}
		if len(docs) > 0 && !yield(entities.Batch{Docs: docs, Cursor: cloneCursor(state)}, nil) {
			return
		}
	}
}

// The document id breaks ties: several issues can share an updated timestamp.
type unitKey struct {
	updatedAt time.Time
	docID     entities.DocID
}

func (k unitKey) compare(o unitKey) int {
	if c := k.updatedAt.Compare(o.updatedAt); c != 0 {
		return c
	}
	return strings.Compare(string(k.docID), string(o.docID))
}

func (k unitKey) after(o unitKey) bool { return k.compare(o) > 0 }

// Comments carry no watermark of their own — Jira bumps the issue's updated when
// a comment changes.
type unit struct {
	key  unitKey
	docs []entities.Document
}

func (c *Connector) units(ctx context.Context, from unitKey) ([]unit, error) {
	var units []unit
	var buildErr error
	err := c.client.eachIssue(ctx, c.jql(from.updatedAt), func(n *issue) bool {
		doc := c.issueDoc(n)
		key := unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID}
		if !key.after(from) {
			return true
		}
		docs, err := c.unitDocs(ctx, n.Key, doc)
		if err != nil {
			buildErr = err
			return false
		}
		units = append(units, unit{key: key, docs: docs})
		return true
	})
	if err != nil {
		return nil, err
	}
	if buildErr != nil {
		return nil, buildErr
	}
	slices.SortFunc(units, func(a, b unit) int { return a.key.compare(b.key) })
	return units, nil
}

func (c *Connector) unitDocs(ctx context.Context, key string, ticket entities.Document) ([]entities.Document, error) {
	comments, err := c.client.comments(ctx, key)
	if err != nil {
		return nil, err
	}
	docs := make([]entities.Document, 0, 1+len(comments))
	docs = append(docs, ticket)
	for i := range comments {
		docs = append(docs, c.commentDoc(key, &comments[i]))
	}
	return docs, nil
}

func (c *Connector) issueDoc(n *issue) entities.Document {
	// The store derives external_key from the third id segment, so the bare key is
	// what makes a "PROJ-123" reference resolve to this document.
	doc := newDocument(entities.DocTypeTicket, n.Key)
	doc.Title = n.Key + ": " + n.Fields.Summary
	doc.Body = flatten(n.Fields.Description)
	doc.Author = n.Fields.Reporter.name()
	doc.URL = c.browseURL(n.Key)
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.Fields.Created.Time, n.Fields.Updated.Time)

	var refs refscan.Set
	addTextRefs(&refs, n.Fields.Summary+"\n"+doc.Body)
	doc.Refs = withoutKey(refs.Refs(), n.Key)
	return doc
}

func (c *Connector) commentDoc(key string, cm *comment) entities.Document {
	// The chunker cuts the id at its last "#" to recover the thread, so the issue
	// key has to sit in front of the only "#" a comment id carries.
	doc := newDocument(entities.DocTypeTicketComment, key+"#"+cm.ID)
	doc.Title = "Comment on " + key
	doc.Body = flatten(cm.Body)
	doc.Author = cm.Author.name()
	doc.URL = c.browseURL(key) + "?focusedCommentId=" + cm.ID
	doc.CreatedAt, doc.UpdatedAt = timestamps(cm.Created.Time, cm.Updated.Time)

	var refs refscan.Set
	refs.Add(entities.RefKindTicketKey, key)
	addTextRefs(&refs, doc.Body)
	doc.Refs = refs.Refs()
	return doc
}

func newDocument(t entities.DocType, externalID string) entities.Document {
	return entities.Document{
		ID:     entities.NewDocID(sourceName, t, externalID),
		Source: sourceName,
		Type:   t,
	}
}

func (c *Connector) browseURL(key string) string { return c.baseURL + "/browse/" + key }

// A JQL datetime literal is minute-granular and read in the requesting user's own
// zone, so jqlSlack dominates the ±14h offset range; units refilters the overlap.
func (c *Connector) jql(watermark time.Time) string {
	var clauses []string
	if len(c.projects) > 0 {
		clauses = append(clauses, "project IN ("+strings.Join(c.projects, ", ")+")")
	}
	if !watermark.IsZero() {
		clauses = append(clauses, `updated >= "`+watermark.Add(-jqlSlack).UTC().Format(jqlTimeLayout)+`"`)
	}
	if len(clauses) == 0 {
		return jqlOrder
	}
	return strings.Join(clauses, " AND ") + " " + jqlOrder
}

func (c *Connector) checkProjectKeys() error {
	for _, key := range c.projects {
		if !validProjectKey(key) {
			return fmt.Errorf("jira: invalid project key %q: want uppercase letters, digits and underscores", key)
		}
	}
	return nil
}

func validProjectKey(key string) bool {
	if key == "" || key[0] < 'A' || key[0] > 'Z' {
		return false
	}
	for _, r := range key {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func addTextRefs(s *refscan.Set, text string) {
	if text == "" {
		return
	}
	s.AddTicketKeys(text)
	s.AddURLs(text)
	s.AddCommitSHAs(text)
	s.AddFilePaths(text)
}

func withoutKey(refs []entities.RawRef, key string) []entities.RawRef {
	return slices.DeleteFunc(refs, func(r entities.RawRef) bool {
		return r.Kind == entities.RefKindTicketKey && r.Value == key
	})
}

func timestamps(created, updated time.Time) (time.Time, time.Time) {
	switch {
	case updated.IsZero():
		return created, created
	case created.IsZero():
		return updated, updated
	}
	return created, updated
}

func readCursor(c entities.Cursor) (unitKey, error) {
	raw := c[cursorUpdatedKey]
	if raw == "" {
		return unitKey{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return unitKey{}, fmt.Errorf("jira: parse cursor watermark %q: %w", raw, err)
	}
	return unitKey{updatedAt: at, docID: entities.DocID(c[cursorDocKey])}, nil
}

// Truncating Jira's milliseconds here would replay the watermark unit on every resume.
func writeCursor(c entities.Cursor, k unitKey) {
	c[cursorUpdatedKey] = k.updatedAt.UTC().Format(time.RFC3339Nano)
	c[cursorDocKey] = string(k.docID)
}

// A yielded batch owns its own map: the caller persists it while the iterator advances.
func cloneCursor(c entities.Cursor) entities.Cursor {
	if len(c) == 0 {
		return entities.Cursor{}
	}
	return maps.Clone(c)
}
