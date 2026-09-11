package gitlab

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/httpx"
	"github.com/setthasit/Lore/sdk/refs"
)

const (
	// forgeName is what `repos[].remote` in lore.yaml is written against; the instance id owns document identity.
	forgeName = "gitlab"

	DefaultBaseURL = "https://gitlab.com"

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
	instance  string
	client    *client
	webRoot   string
	projects  []string
	paths     []string
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

// Each project is a namespaced path ("group/subgroup/project"). An empty baseURL means
// gitlab.com; a self-managed instance passes its root, "https://gitlab.acme.dev".
func NewConnector(instance, token string, projects []string, baseURL string, opts ...Option) *Connector {
	root := httpx.Endpoint(baseURL, DefaultBaseURL, "")
	c := &Connector{
		instance:  instance,
		client:    newClient(root, token),
		webRoot:   root,
		projects:  slices.Clone(projects),
		paths:     projectPaths(projects),
		batchSize: defaultBatchSize,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Connector) Name() string { return c.instance }

func (c *Connector) MatchesRemote(remote string) bool {
	forge, path, ok := lore.SplitRemote(remote)
	if !ok || forge != forgeName {
		return false
	}
	// GitLab paths are case-sensitive; compare verbatim.
	return slices.Contains(c.paths, path)
}

// Changes walks the configured projects in order, oldest-first within each.
func (c *Connector) Changes(ctx context.Context, cursor lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) {
		state := cursor.Clone()

		for _, name := range c.projects {
			p, err := parseProject(name)
			if err != nil {
				yield(lore.Batch{}, err)
				return
			}
			from, err := readCursor(state, p)
			if err != nil {
				yield(lore.Batch{}, err)
				return
			}
			units, err := c.projectUnits(ctx, p, from)
			if err != nil {
				yield(lore.Batch{}, fmt.Errorf("gitlab %s: %w", p.path, err))
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
					writeCursor(state, p, pos)
				}
				if len(docs) < c.batchSize {
					continue
				}
				if !yield(lore.Batch{Docs: docs, Cursor: state.Clone()}, nil) {
					return
				}
				docs = make([]lore.Document, 0, c.batchSize)
			}
			if len(docs) > 0 && !yield(lore.Batch{Docs: docs, Cursor: state.Clone()}, nil) {
				return
			}
		}
	}
}

// encoded is path in the URL-encoded form every /projects/:id endpoint expects.
type project struct {
	path    string
	encoded string
}

func parseProject(s string) (project, error) {
	path := strings.Trim(s, "/")
	if !lore.IsNamespacedPath(path) {
		return project{}, fmt.Errorf("gitlab: invalid project %q: want \"group/project\"", s)
	}
	return project{path: path, encoded: strings.ReplaceAll(path, "/", "%2F")}, nil
}

func projectPaths(projects []string) []string {
	paths := make([]string, 0, len(projects))
	for _, name := range projects {
		if p, err := parseProject(name); err == nil {
			paths = append(paths, p.path)
		}
	}
	return paths
}

func (p project) ref() string { return forgeName + ":" + p.path }

// A bare "#123" or "!123" means nothing outside its project. The sigil is dropped: the
// index keys merge requests and issues by number under one namespace.
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
// their own — GitLab bumps the parent's updated_at when a note changes.
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
	doc := c.newDocument(lore.DocTypeCommit, p, p.path+"/commit/"+n.ID)
	doc.Title = n.Title
	doc.Body = n.Message
	doc.Author = n.AuthorName
	doc.URL = cmp.Or(n.WebURL, c.commitURL(p, n.ID))
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.AuthoredDate, n.CommittedDate)

	diffs, err := c.client.commitDiff(ctx, p, n.ID)
	if err != nil {
		return unit{}, err
	}
	var found refs.Set
	for i := range diffs {
		found.AddAll(lore.RefKindFilePath, changedPaths(&diffs[i]))
	}
	addTextRefs(&found, p, n.Message)
	doc.Refs = found.Refs()

	return unit{
		key:         unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID},
		docs:        []lore.Document{doc},
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
	doc := c.newDocument(lore.DocTypePR, p, external)
	doc.Title = n.Title
	doc.Body = n.Description
	doc.Author = n.Author.display()
	doc.URL = cmp.Or(n.WebURL, c.mergeRequestURL(p, n.IID))
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	commits, err := c.client.mergeRequestCommits(ctx, p, n.IID)
	if err != nil {
		return unit{}, err
	}
	var found refs.Set
	for i := range commits {
		found.Add(lore.RefKindCommitSHA, commits[i].ID)
	}
	found.Add(lore.RefKindCommitSHA, n.SHA)
	found.Add(lore.RefKindCommitSHA, n.MergeCommitSHA)
	found.Add(lore.RefKindCommitSHA, n.SquashCommitSHA)
	// The source branch name carries ticket keys ("feature/PROJ-123-retry").
	addTextRefs(&found, p, n.Title+"\n"+n.Description+"\n"+n.SourceBranch)
	doc.Refs = found.Refs()

	discussions, err := c.client.mergeRequestDiscussions(ctx, p, n.IID)
	if err != nil {
		return unit{}, err
	}
	mr := parent{external: external, url: doc.URL, iid: n.IID}
	docs := []lore.Document{doc}
	for i := range discussions {
		docs = append(docs, c.discussionDocs(p, mr, &discussions[i])...)
	}
	return unit{key: unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID}, docs: docs}, nil
}

type parent struct {
	external string
	url      string
	iid      int
}

func (t parent) noteURL(id int64) string { return t.url + noteFragment(id) }

// A resolvable thread is the closest GitLab has to a review; a standalone comment stays a plain comment.
func (c *Connector) discussionDocs(p project, mr parent, d *discussion) []lore.Document {
	notes := authored(d.Notes)
	if len(notes) == 0 {
		return nil
	}

	docs := make([]lore.Document, 0, len(notes))
	if !d.IndividualNote {
		docs = append(docs, c.reviewDoc(p, mr, notes[0]))
		notes = notes[1:]
	}
	for _, n := range notes {
		docs = append(docs, c.reviewCommentDoc(p, mr, n))
	}
	return docs
}

// GitLab emits a system note per label, milestone and assignee change; none carry reasoning.
func authored(notes []note) []*note {
	out := make([]*note, 0, len(notes))
	for i := range notes {
		if !notes[i].System {
			out = append(out, &notes[i])
		}
	}
	return out
}

func (c *Connector) reviewDoc(p project, mr parent, n *note) lore.Document {
	doc := c.newDocument(lore.DocTypePRReview, p, mr.external+noteFragment(n.ID))
	doc.Title = reviewTitle(p, mr.iid, n)
	doc.Body = n.Body
	doc.Author = n.Author.display()
	doc.URL = mr.noteURL(n.ID)
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	var found refs.Set
	found.AddAll(lore.RefKindFilePath, n.Position.paths())
	c.addOwnedNumberRef(&found, p, mr.iid)
	addTextRefs(&found, p, n.Body)
	doc.Refs = withoutUnscopedDuplicates(found.Refs())
	return doc
}

func (c *Connector) reviewCommentDoc(p project, mr parent, n *note) lore.Document {
	doc := c.newDocument(lore.DocTypeReviewComment, p, mr.external+noteFragment(n.ID))
	doc.Title = reviewCommentTitle(p, mr.iid, n.Position.path())
	doc.Body = n.Body
	doc.Author = n.Author.display()
	doc.URL = mr.noteURL(n.ID)
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	var found refs.Set
	found.AddAll(lore.RefKindFilePath, n.Position.paths())
	c.addOwnedNumberRef(&found, p, mr.iid)
	addTextRefs(&found, p, n.Body)
	doc.Refs = withoutUnscopedDuplicates(found.Refs())
	return doc
}

func (c *Connector) issueUnit(ctx context.Context, p project, n *issue) (unit, error) {
	external := p.path + "/issues/" + strconv.Itoa(n.IID)
	doc := c.newDocument(lore.DocTypeIssue, p, external)
	doc.Title = n.Title
	doc.Body = n.Description
	doc.Author = n.Author.display()
	doc.URL = cmp.Or(n.WebURL, c.issueURL(p, n.IID))
	doc.CreatedAt, doc.UpdatedAt = timestamps(n.CreatedAt, n.UpdatedAt)

	var found refs.Set
	addTextRefs(&found, p, n.Title+"\n"+n.Description)
	doc.Refs = found.Refs()

	notes, err := c.client.issueNotes(ctx, p, n.IID)
	if err != nil {
		return unit{}, err
	}
	docs := []lore.Document{doc}
	for _, cm := range authored(notes) {
		cdoc := c.newDocument(lore.DocTypeIssueComment, p, external+noteFragment(cm.ID))
		cdoc.Title = "Comment on " + p.issueLabel(n.IID)
		cdoc.Body = cm.Body
		cdoc.Author = cm.Author.display()
		cdoc.URL = doc.URL + noteFragment(cm.ID)
		cdoc.CreatedAt, cdoc.UpdatedAt = timestamps(cm.CreatedAt, cm.UpdatedAt)

		var crefs refs.Set
		c.addOwnedNumberRef(&crefs, p, n.IID)
		addTextRefs(&crefs, p, cm.Body)
		cdoc.Refs = withoutUnscopedDuplicates(crefs.Refs())

		docs = append(docs, cdoc)
	}
	return unit{key: unitKey{updatedAt: doc.UpdatedAt, docID: doc.ID}, docs: docs}, nil
}

func (c *Connector) newDocument(t lore.DocType, p project, externalID string) lore.Document {
	return lore.Document{
		ID:      lore.NewDocID(c.instance, t, externalID),
		Source:  c.instance,
		Type:    t,
		RepoRef: p.ref(),
	}
}

func (c *Connector) addOwnedNumberRef(s *refs.Set, p project, number int) {
	s.AddScoped(lore.RefKindPRNumber, p.numberRef(number), c.instance)
}

func withoutUnscopedDuplicates(rs []lore.RawRef) []lore.RawRef {
	var scoped []lore.RawRef
	for _, r := range rs {
		if r.Instance != "" {
			scoped = append(scoped, lore.RawRef{Kind: r.Kind, Value: r.Value})
		}
	}
	if len(scoped) == 0 {
		return rs
	}
	return slices.DeleteFunc(rs, func(r lore.RawRef) bool {
		return r.Instance == "" && slices.Contains(scoped, r)
	})
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
func addTextRefs(s *refs.Set, p project, text string) {
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
		s.Add(lore.RefKindPRNumber, path+"#"+m[2])
	}
	s.AddCommitSHAs(text)
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

func readCursor(c lore.Cursor, p project) (unitKey, error) {
	raw := c[p.path+cursorUpdatedSuffix]
	if raw == "" {
		return unitKey{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return unitKey{}, fmt.Errorf("gitlab %s: parse cursor watermark %q: %w", p.path, raw, err)
	}
	return unitKey{updatedAt: at, docID: lore.DocID(c[p.path+cursorDocSuffix])}, nil
}

// Truncating GitLab's milliseconds here would replay the watermark unit on every resume.
func writeCursor(c lore.Cursor, p project, k unitKey) {
	c[p.path+cursorUpdatedSuffix] = k.updatedAt.UTC().Format(time.RFC3339Nano)
	c[p.path+cursorDocSuffix] = string(k.docID)
}
