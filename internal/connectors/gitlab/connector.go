package gitlab

import (
	"cmp"
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

	"github.com/setthasit/Lore/internal/connectors/httpretry"
	"github.com/setthasit/Lore/internal/connectors/refscan"
	"github.com/setthasit/Lore/internal/entities"
)

const (
	sourceName = "gitlab"

	// DefaultBaseURL is the instance root used when the workspace names none.
	DefaultBaseURL = "https://gitlab.com"

	// defaultBatchSize closes a batch on the next unit boundary, so it may overshoot.
	defaultBatchSize = 50

	cursorUpdatedSuffix = ":updated_at"
	cursorDocSuffix     = ":doc_id"
)

var _ entities.Connector = (*Connector)(nil)

type Connector struct {
	client    *client
	webRoot   string
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

// NewConnector builds a connector for projects, each a namespaced path
// ("group/project" or "group/subgroup/project"). An empty baseURL means
// gitlab.com; a self-managed instance passes its root, "https://gitlab.acme.dev".
func NewConnector(token string, projects []string, baseURL string, opts ...Option) *Connector {
	root := httpretry.Endpoint(baseURL, DefaultBaseURL, "")
	c := &Connector{
		client:    newClient(root, token),
		webRoot:   root,
		projects:  slices.Clone(projects),
		batchSize: defaultBatchSize,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Connector) Name() string { return sourceName }

// Changes walks the configured projects in order, oldest-first within each.
func (c *Connector) Changes(ctx context.Context, cursor entities.Cursor) iter.Seq2[entities.Batch, error] {
	return func(yield func(entities.Batch, error) bool) {
		state := cloneCursor(cursor)

		for _, name := range c.projects {
			p, err := parseProject(name)
			if err != nil {
				yield(entities.Batch{}, err)
				return
			}
			from, err := readCursor(state, p)
			if err != nil {
				yield(entities.Batch{}, err)
				return
			}
			units, err := c.projectUnits(ctx, p, from)
			if err != nil {
				yield(entities.Batch{}, fmt.Errorf("gitlab %s: %w", p.path, err))
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
					writeCursor(state, p, pos)
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

// project is a namespaced path. encoded is the same path in the URL-encoded form
// every /projects/:id endpoint expects.
type project struct {
	path    string
	encoded string
}

func parseProject(s string) (project, error) {
	path := strings.Trim(s, "/")
	segments := strings.Split(path, "/")
	if len(segments) < 2 || slices.Contains(segments, "") {
		return project{}, fmt.Errorf("gitlab: invalid project %q: want \"group/project\"", s)
	}
	return project{path: path, encoded: strings.ReplaceAll(path, "/", "%2F")}, nil
}

func (p project) ref() string { return sourceName + ":" + p.path }

// A bare "#123" or "!123" means nothing outside its project, so it is qualified.
// The sigil is dropped: the index keys merge requests and issues by number under
// one namespace, and a GitLab reference cannot say which of the two it hit any
// more precisely than the resolver already does.
func (p project) numberRef(number int) string {
	return p.path + "#" + strconv.Itoa(number)
}

// Merge requests are written "!123" in GitLab prose; titles follow suit.
func (p project) mrLabel(iid int) string { return p.path + "!" + strconv.Itoa(iid) }

func (p project) issueLabel(iid int) string { return p.path + "#" + strconv.Itoa(iid) }

func (c *Connector) mergeRequestURL(p project, iid int) string {
	return c.webRoot + "/" + p.path + "/-/merge_requests/" + strconv.Itoa(iid)
}

func (c *Connector) issueURL(p project, iid int) string {
	return c.webRoot + "/" + p.path + "/-/issues/" + strconv.Itoa(iid)
}

func (c *Connector) commitURL(p project, sha string) string {
	return c.webRoot + "/" + p.path + "/-/commit/" + sha
}

// The document id breaks ties: several items can share an updated timestamp, and
// a watermark alone cannot tell an already-yielded one from a new one.
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
// their own — GitLab bumps the parent's updated_at when a note changes.
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

// Every collection is filtered server-side by the watermark, so the walk reads
// only what may have changed and sorts the three streams back together.
func (c *Connector) projectUnits(ctx context.Context, p project, from unitKey) ([]unit, error) {
	var since string
	if !from.updatedAt.IsZero() {
		since = from.updatedAt.UTC().Format(time.RFC3339)
	}

	commits, err := c.client.commits(ctx, p, since)
	if err != nil {
		return nil, err
	}
	units := make([]unit, 0, len(commits))
	for i := range commits {
		u, err := c.commitUnit(ctx, p, &commits[i])
		if err != nil {
			return nil, err
		}
		units = append(units, u)
	}

	mrs, err := c.client.mergeRequests(ctx, p, since)
	if err != nil {
		return nil, err
	}
	for i := range mrs {
		u, err := c.mergeRequestUnit(ctx, p, &mrs[i])
		if err != nil {
			return nil, err
		}
		units = append(units, u)
	}

	issues, err := c.client.issues(ctx, p, since)
	if err != nil {
		return nil, err
	}
	for i := range issues {
		u, err := c.issueUnit(ctx, p, &issues[i])
		if err != nil {
			return nil, err
		}
		units = append(units, u)
	}

	slices.SortFunc(units, func(a, b unit) int { return a.key.compare(b.key) })
	return units, nil
}

func (c *Connector) commitUnit(ctx context.Context, p project, n *commit) (unit, error) {
	doc := newDocument(entities.DocTypeCommit, p, p.path+"/commit/"+n.ID)
	doc.Title = n.Title
	doc.Body = n.Message
	doc.Author = n.AuthorName
	doc.URL = cmp.Or(n.WebURL, c.commitURL(p, n.ID))
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.AuthoredDate, n.CommittedDate)

	diffs, err := c.client.commitDiff(ctx, p, n.ID)
	if err != nil {
		return unit{}, err
	}
	var refs refscan.Set
	for i := range diffs {
		refs.AddAll(entities.RefKindFilePath, changedPaths(&diffs[i]))
	}
	addTextRefs(&refs, p, n.Message)
	doc.Refs = refs.Refs()

	return unit{
		key:         unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID},
		docs:        []entities.Document{doc},
		replayOnTie: true,
	}, nil
}

// A rename contributes both paths: blame follows the file, prose cites either.
func changedPaths(d *diff) []string {
	if !d.RenamedFile || d.OldPath == d.NewPath {
		return []string{d.NewPath}
	}
	return []string{d.NewPath, d.OldPath}
}

func (c *Connector) mergeRequestUnit(ctx context.Context, p project, n *mergeRequest) (unit, error) {
	// "/pull/" rather than GitLab's own "/-/merge_requests/": the index resolves a
	// "group/project#123" reference against that external key, whatever the forge.
	external := p.path + "/pull/" + strconv.Itoa(n.IID)
	doc := newDocument(entities.DocTypePR, p, external)
	doc.Title = n.Title
	doc.Body = n.Description
	doc.Author = n.Author.display()
	doc.URL = cmp.Or(n.WebURL, c.mergeRequestURL(p, n.IID))
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	commits, err := c.client.mergeRequestCommits(ctx, p, n.IID)
	if err != nil {
		return unit{}, err
	}
	var refs refscan.Set
	for i := range commits {
		refs.Add(entities.RefKindCommitSHA, commits[i].ID)
	}
	refs.Add(entities.RefKindCommitSHA, n.SHA)
	refs.Add(entities.RefKindCommitSHA, n.MergeCommitSHA)
	refs.Add(entities.RefKindCommitSHA, n.SquashCommitSHA)
	// The source branch name carries ticket keys ("feature/PROJ-123-retry").
	addTextRefs(&refs, p, n.Title+"\n"+n.Description+"\n"+n.SourceBranch)
	doc.Refs = refs.Refs()

	discussions, err := c.client.mergeRequestDiscussions(ctx, p, n.IID)
	if err != nil {
		return unit{}, err
	}
	mr := parent{external: external, url: doc.URL, iid: n.IID}
	docs := []entities.Document{doc}
	for i := range discussions {
		docs = append(docs, discussionDocs(p, mr, &discussions[i])...)
	}
	return unit{key: unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID}, docs: docs}, nil
}

// parent is the merge request a note hangs off: its external id anchors the
// note's own, and its page is where the note's anchor lives.
type parent struct {
	external string
	url      string
	iid      int
}

func (t parent) noteURL(id int64) string { return t.url + noteFragment(id) }

// A resolvable thread is the closest GitLab has to a review: its opening note
// states the position, the replies argue it. A standalone comment opens nothing,
// so it stays a plain review comment.
func discussionDocs(p project, mr parent, d *discussion) []entities.Document {
	notes := authored(d.Notes)
	if len(notes) == 0 {
		return nil
	}

	docs := make([]entities.Document, 0, len(notes))
	if !d.IndividualNote {
		docs = append(docs, reviewDoc(p, mr, notes[0]))
		notes = notes[1:]
	}
	for _, n := range notes {
		docs = append(docs, reviewCommentDoc(p, mr, n))
	}
	return docs
}

// System notes record label, milestone and assignee churn rather than reasoning,
// and GitLab emits one per change: indexing them would bury the argument.
func authored(notes []note) []*note {
	out := make([]*note, 0, len(notes))
	for i := range notes {
		if !notes[i].System {
			out = append(out, &notes[i])
		}
	}
	return out
}

func reviewDoc(p project, mr parent, n *note) entities.Document {
	doc := newDocument(entities.DocTypePRReview, p, mr.external+noteFragment(n.ID))
	doc.Title = reviewTitle(p, mr.iid, n)
	doc.Body = n.Body
	doc.Author = n.Author.display()
	doc.URL = mr.noteURL(n.ID)
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	var refs refscan.Set
	refs.AddAll(entities.RefKindFilePath, n.Position.paths())
	refs.Add(entities.RefKindPRNumber, p.numberRef(mr.iid))
	addTextRefs(&refs, p, n.Body)
	doc.Refs = refs.Refs()
	return doc
}

func reviewCommentDoc(p project, mr parent, n *note) entities.Document {
	doc := newDocument(entities.DocTypeReviewComment, p, mr.external+noteFragment(n.ID))
	doc.Title = reviewCommentTitle(p, mr.iid, n.Position.path())
	doc.Body = n.Body
	doc.Author = n.Author.display()
	doc.URL = mr.noteURL(n.ID)
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	var refs refscan.Set
	refs.AddAll(entities.RefKindFilePath, n.Position.paths())
	refs.Add(entities.RefKindPRNumber, p.numberRef(mr.iid))
	addTextRefs(&refs, p, n.Body)
	doc.Refs = refs.Refs()
	return doc
}

func (c *Connector) issueUnit(ctx context.Context, p project, n *issue) (unit, error) {
	external := p.path + "/issues/" + strconv.Itoa(n.IID)
	doc := newDocument(entities.DocTypeIssue, p, external)
	doc.Title = n.Title
	doc.Body = n.Description
	doc.Author = n.Author.display()
	doc.URL = cmp.Or(n.WebURL, c.issueURL(p, n.IID))
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	var refs refscan.Set
	addTextRefs(&refs, p, n.Title+"\n"+n.Description)
	doc.Refs = refs.Refs()

	notes, err := c.client.issueNotes(ctx, p, n.IID)
	if err != nil {
		return unit{}, err
	}
	docs := []entities.Document{doc}
	for _, cm := range authored(notes) {
		cdoc := newDocument(entities.DocTypeIssueComment, p, external+noteFragment(cm.ID))
		cdoc.Title = "Comment on " + p.issueLabel(n.IID)
		cdoc.Body = cm.Body
		cdoc.Author = cm.Author.display()
		cdoc.URL = doc.URL + noteFragment(cm.ID)
		cdoc.CreatedAt, cdoc.UpdatedAt = timestamps(cm.CreatedAt, cm.UpdatedAt)

		var crefs refscan.Set
		crefs.Add(entities.RefKindPRNumber, p.numberRef(n.IID))
		addTextRefs(&crefs, p, cm.Body)
		cdoc.Refs = crefs.Refs()

		docs = append(docs, cdoc)
	}
	return unit{key: unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID}, docs: docs}, nil
}

func newDocument(t entities.DocType, p project, externalID string) entities.Document {
	return entities.Document{
		ID:      entities.NewDocID(sourceName, t, externalID),
		Source:  sourceName,
		Type:    t,
		RepoRef: p.ref(),
	}
}

// The chunker recovers a note's thread by cutting its external id at the last
// "#", so the fragment has to be the only "#" a note id carries.
func noteFragment(id int64) string { return "#note_" + strconv.FormatInt(id, 10) }

func reviewTitle(p project, iid int, opener *note) string {
	if opener.Resolved {
		return "Review thread (resolved) on " + p.mrLabel(iid)
	}
	return "Review thread on " + p.mrLabel(iid)
}

func reviewCommentTitle(p project, iid int, path string) string {
	title := "Review comment on " + p.mrLabel(iid)
	if path == "" {
		return title
	}
	return title + " (" + path + ")"
}

// crossRefPattern matches GitLab's "#123" (issue), "!123" (merge request) and
// their namespaced forms, whose namespace may nest through subgroups.
var crossRefPattern = regexp.MustCompile(`(?:((?:[A-Za-z0-9][A-Za-z0-9._-]*/)+[A-Za-z0-9][A-Za-z0-9._-]*))?[#!](\d+)`)

// Precision is the resolver's problem: an unresolvable match never becomes an edge.
func addTextRefs(s *refscan.Set, p project, text string) {
	if text == "" {
		return
	}
	s.AddTicketKeys(text)
	s.AddURLs(text)
	for _, m := range crossRefPattern.FindAllStringSubmatch(text, -1) {
		path := m[1]
		if path == "" {
			path = p.path
		}
		s.Add(entities.RefKindPRNumber, path+"#"+m[2])
	}
	s.AddCommitSHAs(text)
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

// A malformed watermark is an error rather than a silent full re-backfill.
func readCursor(c entities.Cursor, p project) (unitKey, error) {
	raw := c[p.path+cursorUpdatedSuffix]
	if raw == "" {
		return unitKey{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return unitKey{}, fmt.Errorf("gitlab %s: parse cursor watermark %q: %w", p.path, raw, err)
	}
	return unitKey{updatedAt: at, docID: entities.DocID(c[p.path+cursorDocSuffix])}, nil
}

// Truncating GitLab's milliseconds here would replay the watermark unit on every resume.
func writeCursor(c entities.Cursor, p project, k unitKey) {
	c[p.path+cursorUpdatedSuffix] = k.updatedAt.UTC().Format(time.RFC3339Nano)
	c[p.path+cursorDocSuffix] = string(k.docID)
}

// A yielded batch owns its own map: the caller persists it while the iterator advances.
func cloneCursor(c entities.Cursor) entities.Cursor {
	if len(c) == 0 {
		return entities.Cursor{}
	}
	return maps.Clone(c)
}
