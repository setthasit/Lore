package services

import (
	"context"
	"fmt"
	"strings"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
)

type refSource interface {
	ResolveRef(ctx context.Context, ref string) ([]entities.DocumentMeta, error)
	DocumentsWithBody(ctx context.Context, ids []entities.DocID) ([]entities.Document, error)
}

// A ref matching nothing reports (zero, false, nil) so a caller may fall back to
// reading it as free text.
func resolveOneRef(ctx context.Context, s refSource, ref string) (entities.DocumentMeta, bool, error) {
	candidates, err := s.ResolveRef(ctx, ref)
	if err != nil {
		return entities.DocumentMeta{}, false, internalerror.NewInternalError("resolving the ref failed", err)
	}

	switch len(candidates) {
	case 0:
		return entities.DocumentMeta{}, false, nil
	case 1:
	default:
		return entities.DocumentMeta{}, false, ambiguousRef(ref, candidates)
	}

	anchor := candidates[0]
	if anchor.URL == "" {
		return entities.DocumentMeta{}, false, internalerror.NewNotFoundError(fmt.Sprintf(
			"ref %q resolved to %s (%s), which carries no citable URL",
			ref, anchor.ID, anchor.Title), nil)
	}

	return anchor, true, nil
}

func ambiguousRef(ref string, candidates []entities.DocumentMeta) error {
	listed := make([]string, len(candidates))
	for i, candidate := range candidates {
		listed[i] = fmt.Sprintf("%s (%s) %s", candidate.ID, candidate.Title, candidate.URL)
	}

	return internalerror.NewBadRequestError(fmt.Sprintf("ref %q matches %d documents — retry with one of: %s",
		ref, len(candidates), strings.Join(listed, "; ")), nil)
}

func documentBody(ctx context.Context, s refSource, id entities.DocID) (string, error) {
	docs, err := s.DocumentsWithBody(ctx, []entities.DocID{id})
	if err != nil {
		return "", internalerror.NewInternalError("loading the document body failed", err)
	}
	for _, doc := range docs {
		if doc.ID == id {
			return doc.Body, nil
		}
	}

	return "", internalerror.NewNotFoundError(fmt.Sprintf("document %s is no longer indexed", id), nil)
}
