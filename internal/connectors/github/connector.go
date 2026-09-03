package github

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/setthasit/Lore/internal/connectors/refscan"
	"github.com/setthasit/Lore/internal/entities"
)

const (
	sourceName = "github"

	// defaultBatchSize closes a batch on the next unit boundary, so it may overshoot.
	defaultBatchSize = 50

	cursorUpdatedSuffix = ":updated_at"
	cursorDocSuffix     = ":doc_id"
)

var _ entities.Connector = (*Connector)(nil)

type Connector struct {
	client    *client
	repos     []string
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

// NewConnector builds a connector for repos ("owner/name" each). An empty baseURL
// means github.com; GitHub Enterprise Server passes its REST root, "https://host/api/v3".
func NewConnector(token string, repos []string, baseURL string, opts ...Option) *Connector {
	c := &Connector{
		client:    newClient(token, baseURL),
		repos:     slices.Clone(repos),
		batchSize: defaultBatchSize,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Connector) Name() string { return sourceName }

// Changes walks the configured repositories in order, oldest-first within each.
func (c *Connector) Changes(ctx context.Context, cursor entities.Cursor) iter.Seq2[entities.Batch, error] {
	return func(yield func(entities.Batch, error) bool) {
		state := cloneCursor(cursor)

		for _, name := range c.repos {
			r, err := parseRepo(name)
			if err != nil {
				yield(entities.Batch{}, err)
				return
			}
			from, err := readCursor(state, r)
			if err != nil {
				yield(entities.Batch{}, err)
				return
			}
			units, err := c.repoUnits(ctx, r, from)
			if err != nil {
				yield(entities.Batch{}, fmt.Errorf("github %s: %w", r.slug, err))
				return
			}

			docs := make([]entities.Document, 0, c.batchSize)
			pos := from
			for i := range units {
				u := &units[i]
				if !u.emitAfter(pos) {
					continue // already yielded under this cursor
				}
				docs = append(docs, u.docs...)
				if u.key.after(pos) {
					pos = u.key
					writeCursor(state, r, pos)
				}
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
}

type repo struct {
	owner string
	name  string
	slug  string
}

func parseRepo(s string) (repo, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return repo{}, fmt.Errorf("github: invalid repository %q: want \"owner/name\"", s)
	}
	return repo{owner: owner, name: name, slug: s}, nil
}

func (r repo) ref() string { return sourceName + ":" + r.slug }

// A "#123" reference means nothing outside its repository, so it is qualified.
func (r repo) numberRef(number int) string {
	return r.slug + "#" + strconv.Itoa(number)
}

// The document id breaks ties: GitHub timestamps have second precision, so a
// watermark alone cannot tell an already-yielded item from a new one.
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

// A unit is a top-level item plus its dependents. Dependents have no watermark of
// their own — GitHub bumps the parent's updatedAt when a comment changes.
type unit struct {
	key  unitKey
	docs []entities.Document

	// replayOnTie exempts the unit from the tiebreak: a commit is immutable and can
	// be pushed long after its committed date, so one lost at a tie never comes back.
	replayOnTie bool
}

func (u *unit) emitAfter(from unitKey) bool {
	if u.replayOnTie {
		return !u.key.updatedAt.Before(from.updatedAt)
	}
	return u.key.after(from)
}

// Connections arrive newest-first, so the changed prefix is buffered and reversed.
func (c *Connector) repoUnits(ctx context.Context, r repo, from unitKey) ([]unit, error) {
	var since any
	if !from.updatedAt.IsZero() {
		since = from.updatedAt.UTC().Format(time.RFC3339)
	}

	commits, err := collectUnits(from,
		func(n *commitNode) time.Time { return n.CommittedDate },
		func(n *commitNode) (unit, error) { return c.commitUnit(ctx, r, n) },
		func(visit func(*commitNode) bool) error { return c.client.eachCommit(ctx, r, since, visit) })
	if err != nil {
		return nil, err
	}

	prs, err := collectUnits(from,
		func(n *prNode) time.Time { return n.UpdatedAt },
		func(n *prNode) (unit, error) { return c.pullRequestUnit(ctx, r, n) },
		func(visit func(*prNode) bool) error { return c.client.eachPullRequest(ctx, r, visit) })
	if err != nil {
		return nil, err
	}

	issues, err := collectUnits(from,
		func(n *issueNode) time.Time { return n.UpdatedAt },
		func(n *issueNode) (unit, error) { return c.issueUnit(ctx, r, n) },
		func(visit func(*issueNode) bool) error { return c.client.eachIssue(ctx, r, since, visit) })
	if err != nil {
		return nil, err
	}

	units := slices.Concat(commits, prs, issues)
	slices.SortFunc(units, func(a, b unit) int { return a.key.compare(b.key) })
	return units, nil
}

// The walk stops at the first item strictly older than the watermark; everything
// behind it is older still. Items sharing that second keep being read.
func collectUnits[T any](
	from unitKey,
	updatedAt func(*T) time.Time,
	build func(*T) (unit, error),
	walk func(visit func(*T) bool) error,
) ([]unit, error) {
	var units []unit
	var buildErr error
	err := walk(func(n *T) bool {
		if updatedAt(n).Before(from.updatedAt) {
			return false
		}
		u, err := build(n)
		if err != nil {
			buildErr = err
			return false
		}
		units = append(units, u)
		return true
	})
	if err != nil {
		return nil, err
	}
	return units, buildErr
}

func (c *Connector) commitUnit(ctx context.Context, r repo, n *commitNode) (unit, error) {
	doc := newDocument(entities.DocTypeCommit, r, r.slug+"/commit/"+n.OID)
	doc.Title = n.MessageHeadline
	doc.Body = n.Message
	doc.Author = n.author()
	doc.URL = n.URL
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.AuthoredDate, n.CommittedDate)

	var refs refscan.Set
	for _, pr := range n.AssociatedPullRequests.Nodes {
		refs.Add(entities.RefKindPRNumber, r.numberRef(pr.Number))
	}
	if n.touchesFiles() {
		paths, err := c.client.commitFiles(ctx, r, n.OID)
		if err != nil {
			return unit{}, fmt.Errorf("commit %s: %w", n.OID, err)
		}
		refs.AddAll(entities.RefKindFilePath, paths)
	}
	addTextRefs(&refs, r, n.Message)
	doc.Refs = refs.Refs()

	return unit{
		key:         unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID},
		docs:        []entities.Document{doc},
		replayOnTie: true,
	}, nil
}

func (c *Connector) pullRequestUnit(ctx context.Context, r repo, n *prNode) (unit, error) {
	external := r.slug + "/pull/" + strconv.Itoa(n.Number)
	doc := newDocument(entities.DocTypePR, r, external)
	doc.Title = n.Title
	doc.Body = n.Body
	doc.Author = n.Author.login()
	doc.URL = n.URL
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	var refs refscan.Set
	for _, issue := range n.ClosingIssuesReferences.Nodes {
		refs.Add(entities.RefKindPRNumber, r.numberRef(issue.Number))
	}
	oids, err := c.client.commitOIDs(ctx, r, n.Number, n.Commits)
	if err != nil {
		return unit{}, err
	}
	refs.AddAll(entities.RefKindCommitSHA, oids)
	// The head branch name carries ticket keys ("feature/PROJ-123-retry").
	addTextRefs(&refs, r, n.Title+"\n"+n.Body+"\n"+n.HeadRefName)
	doc.Refs = refs.Refs()

	reviews, err := c.client.reviews(ctx, r, n.Number, n.Reviews)
	if err != nil {
		return unit{}, err
	}
	docs := make([]entities.Document, 0, 1+len(reviews))
	docs = append(docs, doc)
	for i := range reviews {
		reviewDocs, err := c.reviewDocs(ctx, r, external, n.Number, &reviews[i])
		if err != nil {
			return unit{}, err
		}
		docs = append(docs, reviewDocs...)
	}
	return unit{key: unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID}, docs: docs}, nil
}

func (c *Connector) reviewDocs(ctx context.Context, r repo, prExternal string, number int, rv *reviewNode) ([]entities.Document, error) {
	doc := newDocument(entities.DocTypePRReview, r, prExternal+commentFragment(rv.URL, "pullrequestreview-", rv.DatabaseID))
	doc.Title = reviewTitle(r, number, rv.State)
	doc.Body = rv.Body
	doc.Author = rv.Author.login()
	doc.URL = rv.URL
	doc.CreatedAt, doc.UpdatedAt = timestamps(rv.CreatedAt, rv.UpdatedAt)

	var refs refscan.Set
	refs.Add(entities.RefKindPRNumber, r.numberRef(number))
	addTextRefs(&refs, r, rv.Body)
	doc.Refs = refs.Refs()

	comments, err := c.client.reviewComments(ctx, rv.ID, rv.Comments)
	if err != nil {
		return nil, err
	}
	docs := make([]entities.Document, 0, 1+len(comments))
	docs = append(docs, doc)
	for i := range comments {
		cm := &comments[i]
		cdoc := newDocument(entities.DocTypeReviewComment, r, prExternal+commentFragment(cm.URL, "discussion_r", cm.DatabaseID))
		cdoc.Title = reviewCommentTitle(r, number, cm.Path)
		cdoc.Body = cm.Body
		cdoc.Author = cm.Author.login()
		cdoc.URL = cm.URL
		cdoc.CreatedAt, cdoc.UpdatedAt = timestamps(cm.CreatedAt, cm.UpdatedAt)

		var crefs refscan.Set
		crefs.Add(entities.RefKindFilePath, cm.Path)
		crefs.Add(entities.RefKindPRNumber, r.numberRef(number))
		addTextRefs(&crefs, r, cm.Body)
		cdoc.Refs = crefs.Refs()

		docs = append(docs, cdoc)
	}
	return docs, nil
}

func (c *Connector) issueUnit(ctx context.Context, r repo, n *issueNode) (unit, error) {
	external := r.slug + "/issues/" + strconv.Itoa(n.Number)
	doc := newDocument(entities.DocTypeIssue, r, external)
	doc.Title = n.Title
	doc.Body = n.Body
	doc.Author = n.Author.login()
	doc.URL = n.URL
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	var refs refscan.Set
	addTextRefs(&refs, r, n.Title+"\n"+n.Body)
	doc.Refs = refs.Refs()

	comments, err := c.client.issueComments(ctx, r, n.Number, n.Comments)
	if err != nil {
		return unit{}, err
	}
	docs := make([]entities.Document, 0, 1+len(comments))
	docs = append(docs, doc)
	for i := range comments {
		cm := &comments[i]
		cdoc := newDocument(entities.DocTypeIssueComment, r, external+commentFragment(cm.URL, "issuecomment-", cm.DatabaseID))
		cdoc.Title = "Comment on " + r.numberRef(n.Number)
		cdoc.Body = cm.Body
		cdoc.Author = cm.Author.login()
		cdoc.URL = cm.URL
		cdoc.CreatedAt, cdoc.UpdatedAt = timestamps(cm.CreatedAt, cm.UpdatedAt)

		var crefs refscan.Set
		crefs.Add(entities.RefKindPRNumber, r.numberRef(n.Number))
		addTextRefs(&crefs, r, cm.Body)
		cdoc.Refs = crefs.Refs()

		docs = append(docs, cdoc)
	}
	return unit{key: unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID}, docs: docs}, nil
}

func newDocument(t entities.DocType, r repo, externalID string) entities.Document {
	return entities.Document{
		ID:      entities.NewDocID(sourceName, t, externalID),
		Source:  sourceName,
		Type:    t,
		RepoRef: r.ref(),
	}
}

// Either timestamp fills from the other when the source left one empty.
func timestamps(created, updated time.Time) (time.Time, time.Time) {
	switch {
	case updated.IsZero():
		return created, created
	case created.IsZero():
		return updated, updated
	}
	return created, updated
}

// The API URL carries the fragment; the database id is the fallback when it is absent.
func commentFragment(url, prefix string, databaseID int64) string {
	if _, fragment, ok := strings.Cut(url, "#"); ok && fragment != "" {
		return "#" + fragment
	}
	return "#" + prefix + strconv.FormatInt(databaseID, 10)
}

func reviewTitle(r repo, number int, state string) string {
	if state == "" {
		return "Review on " + r.numberRef(number)
	}
	return "Review (" + state + ") on " + r.numberRef(number)
}

func reviewCommentTitle(r repo, number int, path string) string {
	title := "Review comment on " + r.numberRef(number)
	if path == "" {
		return title
	}
	return title + " (" + path + ")"
}

// crossRefPattern matches "#123" and "owner/repo#123".
var crossRefPattern = regexp.MustCompile(`(?:([A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*))?#(\d+)`)

// Precision is the resolver's problem: an unresolvable match never becomes an edge.
func addTextRefs(s *refscan.Set, r repo, text string) {
	if text == "" {
		return
	}
	s.AddTicketKeys(text)
	s.AddURLs(text)
	for _, m := range crossRefPattern.FindAllStringSubmatch(text, -1) {
		slug := m[1]
		if slug == "" {
			slug = r.slug
		}
		s.Add(entities.RefKindPRNumber, slug+"#"+m[2])
	}
	s.AddCommitSHAs(text)
}

// A malformed watermark is an error rather than a silent full re-backfill.
func readCursor(c entities.Cursor, r repo) (unitKey, error) {
	raw := c[r.slug+cursorUpdatedSuffix]
	if raw == "" {
		return unitKey{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return unitKey{}, fmt.Errorf("github %s: parse cursor watermark %q: %w", r.slug, raw, err)
	}
	return unitKey{updatedAt: at, docID: entities.DocID(c[r.slug+cursorDocSuffix])}, nil
}

func writeCursor(c entities.Cursor, r repo, k unitKey) {
	c[r.slug+cursorUpdatedSuffix] = k.updatedAt.UTC().Format(time.RFC3339)
	c[r.slug+cursorDocSuffix] = string(k.docID)
}

// A yielded batch owns its own map: the caller persists it while the iterator advances.
func cloneCursor(c entities.Cursor) entities.Cursor {
	if len(c) == 0 {
		return entities.Cursor{}
	}
	return maps.Clone(c)
}
