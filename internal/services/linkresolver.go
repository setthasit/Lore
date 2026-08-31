package services

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/repositories"
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

type linkResolver struct {
	store repositories.IndexStore
}

var _ LinkResolver = (*linkResolver)(nil)

func NewLinkResolver(store repositories.IndexStore) LinkResolver {
	return &linkResolver{store: store}
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
	)
	for _, ref := range refs {
		target, ok, err := l.target(ctx, ref)
		if err != nil {
			return err
		}
		if !ok {
			pending = append(pending, ref)
			continue
		}
		resolved = append(resolved, resolvedRef{ref: ref, target: target})
	}

	sources, err := l.sourceDocuments(ctx, resolved, inHand)
	if err != nil {
		return err
	}

	edges := make([]entities.Edge, len(resolved))
	done := make([]entities.PendingRef, len(resolved))
	for i, r := range resolved {
		edges[i] = edgeFor(sources[r.ref.SourceDoc], r)
		done[i] = r.ref
	}

	return l.commit(ctx, edges, pending, done)
}

func (l *linkResolver) target(ctx context.Context, ref entities.PendingRef) (entities.DocumentMeta, bool, error) {
	if ref.Ref.Kind == entities.RefKindFilePath {
		return entities.DocumentMeta{}, false, nil
	}

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
	rule, explicit := explicitRelation(source.Type, r.target.Type)
	if !explicit {
		rule = textRule(source, r.ref.Ref)
	}

	return entities.Edge{
		Src:        r.ref.SourceDoc,
		Dst:        r.target.ID,
		Kind:       rule.kind,
		Confidence: rule.confidence,
	}
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
