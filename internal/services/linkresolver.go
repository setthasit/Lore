package services

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/repositories"
)

type LinkResolver interface {
	// Refs of documents whose target is not ingested yet are recorded for retry.
	Link(ctx context.Context, docs []entities.Document) error
	LinkPending(ctx context.Context) error
}

type edgeRule struct {
	kind       entities.EdgeKind
	confidence float32
}

var (
	commitInPRRule    = edgeRule{entities.EdgeKindCommitInPR, 1.0}
	prClosesIssueRule = edgeRule{entities.EdgeKindPRClosesIssue, 1.0}
	supersedesRule    = edgeRule{entities.EdgeKindSupersedes, 0.8}
)

var refKindRules = map[entities.RefKind]edgeRule{
	entities.RefKindURL:       {entities.EdgeKindReferencesDoc, 1.0},
	entities.RefKindCommitSHA: {entities.EdgeKindMentionsCommit, 0.9},
	entities.RefKindTicketKey: {entities.EdgeKindReferencesDoc, 0.9},
	entities.RefKindPRNumber:  {entities.EdgeKindReferencesDoc, 0.9},
	entities.RefKindFilePath:  {entities.EdgeKindMentionsPath, 0.7},
}

var refTargetTypes = map[entities.RefKind][]entities.DocType{
	entities.RefKindCommitSHA: {entities.DocTypeCommit},
	entities.RefKindPRNumber:  {entities.DocTypePR, entities.DocTypeIssue},
	entities.RefKindTicketKey: {entities.DocTypeTicket, entities.DocTypeIssue},
}

var supersedePhrases = []string{"supersede", "replaces", "replaced by"}

// A long-lived file's log runs to thousands of commits; only its recent history
// is plausibly what a document naming the path is about.
const maxPathCommits = 50

type linkResolver struct {
	store repositories.IndexStore
	repos []CodeRepo
}

var _ LinkResolver = (*linkResolver)(nil)

func NewLinkResolver(store repositories.IndexStore, repos []CodeRepo) LinkResolver {
	return &linkResolver{store: store, repos: slices.Clone(repos)}
}

func (l *linkResolver) Link(ctx context.Context, docs []entities.Document) error {
	sources := make(map[entities.DocID]entities.Document, len(docs))
	var refs []entities.PendingRef
	for _, doc := range docs {
		sources[doc.ID] = doc
		for _, ref := range doc.Refs {
			refs = append(refs, entities.PendingRef{SourceDoc: doc.ID, Ref: ref})
		}
	}

	return l.resolve(ctx, refs, sources)
}

func (l *linkResolver) LinkPending(ctx context.Context) error {
	refs, err := l.store.PendingRefs(ctx)
	if err != nil {
		return internalerror.NewInternalError("could not read the pending references", err)
	}

	return l.resolve(ctx, refs, nil)
}

type resolvedRef struct {
	ref    entities.PendingRef
	target entities.DocumentMeta
}

func (l *linkResolver) resolve(ctx context.Context, refs []entities.PendingRef, inHand map[entities.DocID]entities.Document) error {
	if len(refs) == 0 {
		return nil
	}

	var (
		pending  []entities.PendingRef
		resolved []resolvedRef
		done     []entities.PendingRef
		err      error
	)
	logged := map[loggedPath][]entities.DocumentMeta{}
	for _, ref := range refs {
		before := len(resolved)
		if resolved, err = l.appendTargets(ctx, resolved, logged, ref); err != nil {
			return err
		}
		if len(resolved) == before {
			pending = append(pending, ref)
			continue
		}
		done = append(done, ref)
	}

	sources, err := l.sourceDocuments(ctx, resolved, inHand)
	if err != nil {
		return err
	}

	edges := make([]entities.Edge, len(resolved))
	for i, r := range resolved {
		edges[i] = edgeFor(sources[r.ref.SourceDoc], r)
	}

	return l.commit(ctx, edges, pending, done)
}

// A path names every commit that touched it, so one ref yields many targets.
func (l *linkResolver) appendTargets(
	ctx context.Context,
	into []resolvedRef,
	logged map[loggedPath][]entities.DocumentMeta,
	ref entities.PendingRef,
) ([]resolvedRef, error) {
	if ref.Ref.Kind == entities.RefKindFilePath {
		commits, err := l.pathCommits(ctx, logged, ref.Ref.Value)
		if err != nil {
			return into, err
		}
		for _, commit := range commits {
			if commit.ID != ref.SourceDoc {
				into = append(into, resolvedRef{ref: ref, target: commit})
			}
		}

		return into, nil
	}

	target, ok, err := l.target(ctx, ref)
	if err != nil || !ok {
		return into, err
	}

	return append(into, resolvedRef{ref: ref, target: target}), nil
}

// Keyed for one resolve call only: a cache outliving the round would miss new commits.
type loggedPath struct {
	repo string
	path string
}

func (l *linkResolver) pathCommits(
	ctx context.Context,
	logged map[loggedPath][]entities.DocumentMeta,
	path string,
) ([]entities.DocumentMeta, error) {
	var commits []entities.DocumentMeta
	for _, repo := range l.repos {
		key := loggedPath{repo: repo.name(), path: path}
		touching, cached := logged[key]
		if !cached {
			var err error
			if touching, err = l.commitsTouching(ctx, repo, path); err != nil {
				return nil, err
			}
			logged[key] = touching
		}
		commits = append(commits, touching...)
	}

	return commits, nil
}

// A clone git cannot read leaves the path pending for a later round, never failing the sync.
func (l *linkResolver) commitsTouching(ctx context.Context, repo CodeRepo, path string) ([]entities.DocumentMeta, error) {
	tracked, err := repo.Git.HasFileAtHEAD(ctx, path)
	if err != nil || !tracked {
		return nil, nil
	}

	history, err := repo.Git.Log(ctx, path)
	if err != nil {
		return nil, nil
	}

	var commits []entities.DocumentMeta
	for _, entry := range history[:min(len(history), maxPathCommits)] {
		indexed, err := indexedCommits(ctx, l.store, entry.SHA)
		if err != nil {
			return nil, internalerror.NewInternalError(
				fmt.Sprintf("could not resolve the commit %s touching %s", shortSHA(entry.SHA), path), err)
		}
		commits = append(commits, indexed...)
	}

	return commits, nil
}

func (l *linkResolver) target(ctx context.Context, ref entities.PendingRef) (entities.DocumentMeta, bool, error) {
	candidates, err := l.store.ResolveRef(ctx, ref.Ref.Value)
	if err != nil {
		return entities.DocumentMeta{}, false, internalerror.NewInternalError(
			fmt.Sprintf("could not resolve the reference %q", ref.Ref.Value), err)
	}

	var only entities.DocumentMeta
	found := 0
	for _, c := range candidates {
		if c.ID == ref.SourceDoc || !admitsTarget(ref.Ref.Kind, c.Type) {
			continue
		}
		found++
		if found > 1 {
			return entities.DocumentMeta{}, false, nil
		}
		only = c
	}

	return only, found == 1, nil
}

func admitsTarget(kind entities.RefKind, target entities.DocType) bool {
	if kind == entities.RefKindURL {
		return true
	}

	return slices.Contains(refTargetTypes[kind], target)
}

func (l *linkResolver) sourceDocuments(
	ctx context.Context,
	resolved []resolvedRef,
	inHand map[entities.DocID]entities.Document,
) (map[entities.DocID]entities.Document, error) {
	sources := make(map[entities.DocID]entities.Document, len(resolved))
	var absent []entities.DocID
	for _, r := range resolved {
		id := r.ref.SourceDoc
		if _, seen := sources[id]; seen {
			continue
		}
		if doc, ok := inHand[id]; ok {
			sources[id] = doc
			continue
		}
		sources[id] = entities.Document{}
		absent = append(absent, id)
	}

	if len(absent) == 0 {
		return sources, nil
	}

	loaded, err := l.store.DocumentsWithBody(ctx, absent)
	if err != nil {
		return nil, internalerror.NewInternalError(fmt.Sprintf(
			"could not read the %d documents referencing a resolved target", len(absent)), err)
	}
	for _, doc := range loaded {
		sources[doc.ID] = doc
	}

	return sources, nil
}

func edgeFor(source entities.Document, r resolvedRef) entities.Edge {
	rule := ruleFor(source, r)

	return entities.Edge{
		Src:        r.ref.SourceDoc,
		Dst:        r.target.ID,
		Kind:       rule.kind,
		Confidence: rule.confidence,
	}
}

// Neither the document type pair nor a supersede phrase qualifies a path ref: its
// targets are commits the path itself picked, not documents the body talks about.
func ruleFor(source entities.Document, r resolvedRef) edgeRule {
	if kind := r.ref.Ref.Kind; kind == entities.RefKindFilePath {
		return refKindRules[kind]
	}
	if rule, explicit := explicitRelation(source.Type, r.target.Type); explicit {
		return rule
	}

	return textRule(source, r.ref.Ref)
}

// Connectors emit API relations and body-text matches under the same RefKind, so
// the document type pair is the only thing that tells the two apart.
func explicitRelation(source, target entities.DocType) (edgeRule, bool) {
	switch {
	case source == entities.DocTypeCommit && target == entities.DocTypePR,
		source == entities.DocTypePR && target == entities.DocTypeCommit:
		return commitInPRRule, true
	case source == entities.DocTypePR && target == entities.DocTypeIssue:
		return prClosesIssueRule, true
	}

	return edgeRule{}, false
}

func textRule(source entities.Document, ref entities.RawRef) edgeRule {
	if hasSupersedePhrase(source, ref) {
		return supersedesRule
	}

	return refKindRules[ref.Kind]
}

func hasSupersedePhrase(source entities.Document, ref entities.RawRef) bool {
	form := strings.ToLower(referenceBodyForm(ref))
	for _, text := range [...]string{source.Title, source.Body} {
		for line := range strings.Lines(text) {
			if supersedesReference(strings.ToLower(line), form) {
				return true
			}
		}
	}

	return false
}

func supersedesReference(line, form string) bool {
	if !mentionsForm(line, form) {
		return false
	}

	for _, phrase := range supersedePhrases {
		if strings.Contains(line, phrase) {
			return true
		}
	}

	return false
}

// "#12" inside "#123" names a different reference, so a match needs both ends free.
func mentionsForm(line, form string) bool {
	if form == "" {
		return false
	}

	for at := 0; at+len(form) <= len(line); {
		i := strings.Index(line[at:], form)
		if i < 0 {
			return false
		}
		i += at
		if !alphanumericAt(line, i-1) && !alphanumericAt(line, i+len(form)) {
			return true
		}
		at = i + 1
	}

	return false
}

func alphanumericAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}

	c := s[i]
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// A pr_number ref is repo-qualified ("acme/lore#123"); prose writes "#123".
func referenceBodyForm(ref entities.RawRef) string {
	if ref.Kind != entities.RefKindPRNumber {
		return ref.Value
	}

	if _, number, ok := strings.Cut(ref.Value, "#"); ok {
		return "#" + number
	}

	return ref.Value
}

func (l *linkResolver) commit(ctx context.Context, edges []entities.Edge, pending, done []entities.PendingRef) error {
	if len(edges) > 0 {
		if err := l.store.UpsertEdges(ctx, edges); err != nil {
			return internalerror.NewInternalError(
				fmt.Sprintf("could not store %d resolved reference edges", len(edges)), err)
		}
	}

	if len(pending) > 0 {
		if err := l.store.UpsertPendingRefs(ctx, pending); err != nil {
			return internalerror.NewInternalError(
				fmt.Sprintf("could not record %d references for a later round", len(pending)), err)
		}
	}

	if len(done) > 0 {
		if err := l.store.DeletePendingRefs(ctx, done); err != nil {
			return internalerror.NewInternalError(
				fmt.Sprintf("could not clear %d resolved pending references", len(done)), err)
		}
	}

	return nil
}
