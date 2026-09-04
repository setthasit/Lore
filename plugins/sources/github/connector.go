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

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/refs"
)

const (
	// forgeName is the identity of the forge, not of this connector: it is what
	// `repos[].remote` in lore.yaml is written against, so it stays fixed while
	// the instance id — which prefixes document identity — is operator-chosen.
	forgeName = "github"

	// defaultBatchSize closes a batch on the next unit boundary, so it may overshoot.
	defaultBatchSize = 50

	cursorUpdatedSuffix = ":updated_at"
	cursorDocSuffix     = ":doc_id"
)

var (
	_ lore.Connector     = (*Connector)(nil)
	_ lore.RemoteMatcher = (*Connector)(nil)
)

type Connector struct {
	client    *client
	instance  string
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

// NewConnector builds a connector for repos ("owner/name" each) under the source
// instance id, which prefixes every document identity it produces. An empty
// baseURL means github.com; GitHub Enterprise Server passes its REST root,
// "https://host/api/v3".
func NewConnector(instance, token string, repos []string, baseURL string, opts ...Option) *Connector {
	c := &Connector{
		client:    newClient(token, baseURL),
		instance:  instance,
		repos:     slices.Clone(repos),
		batchSize: defaultBatchSize,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Connector) Name() string { return c.instance }

// MatchesRemote answers whether a registered local clone belongs to a repository
// this instance ingests, which is what keeps the startup warning about an
// unmatched clone working without the engine knowing a forge by name.
func (c *Connector) MatchesRemote(remote string) bool {
	forge, path, ok := strings.Cut(remote, ":")
	if !ok || forge != forgeName || !namespacedPath(path) {
		return false
	}
	// GitHub owner and repository names are case-insensitive.
	return slices.ContainsFunc(c.repos, func(ingested string) bool {
		return strings.EqualFold(ingested, path)
	})
}

// A repository path is at least a namespace and a name. Depth beyond that is the
// forge's business: an entry no configured repo lists still fails to match.
func namespacedPath(path string) bool {
	segments := strings.Split(path, "/")
	return len(segments) >= 2 && !slices.Contains(segments, "")
}

// Changes walks the configured repositories in order, oldest-first within each.
func (c *Connector) Changes(ctx context.Context, cursor lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) {
		state := cloneCursor(cursor)

		for _, name := range c.repos {
			r, err := parseRepo(name)
			if err != nil {
				yield(lore.Batch{}, err)
				return
			}
			from, err := readCursor(state, r)
			if err != nil {
				yield(lore.Batch{}, err)
				return
			}
			units, err := c.repoUnits(ctx, r, from)
			if err != nil {
				yield(lore.Batch{}, fmt.Errorf("github %s: %w", r.slug, err))
				return
			}

			docs := make([]lore.Document, 0, c.batchSize)
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
				if !yield(lore.Batch{Docs: docs, Cursor: cloneCursor(state)}, nil) {
					return
				}
				docs = make([]lore.Document, 0, c.batchSize)
			}
			if len(docs) > 0 && !yield(lore.Batch{Docs: docs, Cursor: cloneCursor(state)}, nil) {
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

func (r repo) ref() string { return forgeName + ":" + r.slug }

// A "#123" reference means nothing outside its repository, so it is qualified.
func (r repo) numberRef(number int) string {
	return r.slug + "#" + strconv.Itoa(number)
}

// The document id breaks ties: GitHub timestamps have second precision, so a
// watermark alone cannot tell an already-yielded item from a new one.
type unitKey struct {
	updatedAt time.Time
	docID     lore.DocID
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
	docs []lore.Document

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
	doc := c.newDocument(lore.DocTypeCommit, r, r.slug+"/commit/"+n.OID)
	doc.Title = n.MessageHeadline
	doc.Body = n.Message
	doc.Author = n.author()
	doc.URL = n.URL
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.AuthoredDate, n.CommittedDate)

	var found refs.Set
	for _, pr := range n.AssociatedPullRequests.Nodes {
		found.Add(lore.RefKindPRNumber, r.numberRef(pr.Number))
	}
	if n.touchesFiles() {
		paths, err := c.client.commitFiles(ctx, r, n.OID)
		if err != nil {
			return unit{}, fmt.Errorf("commit %s: %w", n.OID, err)
		}
		found.AddAll(lore.RefKindFilePath, paths)
	}
	addTextRefs(&found, r, n.Message)
	doc.Refs = found.Refs()

	return unit{
		key:         unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID},
		docs:        []lore.Document{doc},
		replayOnTie: true,
	}, nil
}

func (c *Connector) pullRequestUnit(ctx context.Context, r repo, n *prNode) (unit, error) {
	external := r.slug + "/pull/" + strconv.Itoa(n.Number)
	doc := c.newDocument(lore.DocTypePR, r, external)
	doc.Title = n.Title
	doc.Body = n.Body
	doc.Author = n.Author.login()
	doc.URL = n.URL
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	var found refs.Set
	for _, issue := range n.ClosingIssuesReferences.Nodes {
		found.Add(lore.RefKindPRNumber, r.numberRef(issue.Number))
	}
	oids, err := c.client.commitOIDs(ctx, r, n.Number, n.Commits)
	if err != nil {
		return unit{}, err
	}
	found.AddAll(lore.RefKindCommitSHA, oids)
	// The head branch name carries ticket keys ("feature/PROJ-123-retry").
	addTextRefs(&found, r, n.Title+"\n"+n.Body+"\n"+n.HeadRefName)
	doc.Refs = found.Refs()

	reviews, err := c.client.reviews(ctx, r, n.Number, n.Reviews)
	if err != nil {
		return unit{}, err
	}
	docs := make([]lore.Document, 0, 1+len(reviews))
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

func (c *Connector) reviewDocs(ctx context.Context, r repo, prExternal string, number int, rv *reviewNode) ([]lore.Document, error) {
	doc := c.newDocument(lore.DocTypePRReview, r, prExternal+commentFragment(rv.URL, "pullrequestreview-", rv.DatabaseID))
	doc.Title = reviewTitle(r, number, rv.State)
	doc.Body = rv.Body
	doc.Author = rv.Author.login()
	doc.URL = rv.URL
	doc.CreatedAt, doc.UpdatedAt = timestamps(rv.CreatedAt, rv.UpdatedAt)

	var found refs.Set
	found.Add(lore.RefKindPRNumber, r.numberRef(number))
	addTextRefs(&found, r, rv.Body)
	doc.Refs = found.Refs()

	comments, err := c.client.reviewComments(ctx, rv.ID, rv.Comments)
	if err != nil {
		return nil, err
	}
	docs := make([]lore.Document, 0, 1+len(comments))
	docs = append(docs, doc)
	for i := range comments {
		cm := &comments[i]
		cdoc := c.newDocument(lore.DocTypeReviewComment, r, prExternal+commentFragment(cm.URL, "discussion_r", cm.DatabaseID))
		cdoc.Title = reviewCommentTitle(r, number, cm.Path)
		cdoc.Body = cm.Body
		cdoc.Author = cm.Author.login()
		cdoc.URL = cm.URL
		cdoc.CreatedAt, cdoc.UpdatedAt = timestamps(cm.CreatedAt, cm.UpdatedAt)

		var crefs refs.Set
		crefs.Add(lore.RefKindFilePath, cm.Path)
		crefs.Add(lore.RefKindPRNumber, r.numberRef(number))
		addTextRefs(&crefs, r, cm.Body)
		cdoc.Refs = crefs.Refs()

		docs = append(docs, cdoc)
	}
	return docs, nil
}

func (c *Connector) issueUnit(ctx context.Context, r repo, n *issueNode) (unit, error) {
	external := r.slug + "/issues/" + strconv.Itoa(n.Number)
	doc := c.newDocument(lore.DocTypeIssue, r, external)
	doc.Title = n.Title
	doc.Body = n.Body
	doc.Author = n.Author.login()
	doc.URL = n.URL
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	var found refs.Set
	addTextRefs(&found, r, n.Title+"\n"+n.Body)
	doc.Refs = found.Refs()

	comments, err := c.client.issueComments(ctx, r, n.Number, n.Comments)
	if err != nil {
		return unit{}, err
	}
	docs := make([]lore.Document, 0, 1+len(comments))
	docs = append(docs, doc)
	for i := range comments {
		cm := &comments[i]
		cdoc := c.newDocument(lore.DocTypeIssueComment, r, external+commentFragment(cm.URL, "issuecomment-", cm.DatabaseID))
		cdoc.Title = "Comment on " + r.numberRef(n.Number)
		cdoc.Body = cm.Body
		cdoc.Author = cm.Author.login()
		cdoc.URL = cm.URL
		cdoc.CreatedAt, cdoc.UpdatedAt = timestamps(cm.CreatedAt, cm.UpdatedAt)

		var crefs refs.Set
		crefs.Add(lore.RefKindPRNumber, r.numberRef(n.Number))
		addTextRefs(&crefs, r, cm.Body)
		cdoc.Refs = crefs.Refs()

		docs = append(docs, cdoc)
	}
	return unit{key: unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID}, docs: docs}, nil
}

// The instance id carries document identity while RepoRef carries the forge
// name, because a clone's remote in lore.yaml names the forge, not the instance.
func (c *Connector) newDocument(t lore.DocType, r repo, externalID string) lore.Document {
	return lore.Document{
		ID:      lore.NewDocID(c.instance, t, externalID),
		Source:  c.instance,
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
func addTextRefs(s *refs.Set, r repo, text string) {
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
		s.Add(lore.RefKindPRNumber, slug+"#"+m[2])
	}
	s.AddCommitSHAs(text)
}

// A malformed watermark is an error rather than a silent full re-backfill.
func readCursor(c lore.Cursor, r repo) (unitKey, error) {
	raw := c[r.slug+cursorUpdatedSuffix]
	if raw == "" {
		return unitKey{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return unitKey{}, fmt.Errorf("github %s: parse cursor watermark %q: %w", r.slug, raw, err)
	}
	return unitKey{updatedAt: at, docID: lore.DocID(c[r.slug+cursorDocSuffix])}, nil
}

func writeCursor(c lore.Cursor, r repo, k unitKey) {
	c[r.slug+cursorUpdatedSuffix] = k.updatedAt.UTC().Format(time.RFC3339)
	c[r.slug+cursorDocSuffix] = string(k.docID)
}

// A yielded batch owns its own map: the caller persists it while the iterator advances.
func cloneCursor(c lore.Cursor) lore.Cursor {
	if len(c) == 0 {
		return lore.Cursor{}
	}
	return maps.Clone(c)
}
