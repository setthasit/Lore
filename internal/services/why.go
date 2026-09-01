package services

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
)

type WhyService interface {
	// Repo names a registered repo by remote or by path; empty selects the only one registered.
	Why(ctx context.Context, req WhyRequest) (*entities.EvidenceBundle, error)
}

type WhyRequest struct {
	Repo      string
	File      string
	LineStart int
	LineEnd   int
	Question  string
}

// Remote is the "github:acme/lore" name mapping the clone onto a source repo.
type CodeRepo struct {
	Path   string
	Remote string
}

const (
	askOnlyRefusal = "no repositories registered — code anchoring disabled for this workspace"
	blameUnbuilt   = "code anchoring needs local-clone blame, which this build does not provide — ask find_decision instead, which needs no code anchor"
)

type whyService struct {
	repos []CodeRepo
}

var _ WhyService = (*whyService)(nil)

func NewWhyService(repos []CodeRepo) WhyService {
	return &whyService{repos: slices.Clone(repos)}
}

func (w *whyService) Why(_ context.Context, req WhyRequest) (*entities.EvidenceBundle, error) {
	if len(w.repos) == 0 {
		return nil, internalerror.NewPreconditionError(askOnlyRefusal, nil)
	}
	if err := w.validateRepo(req.Repo); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.File) == "" {
		return nil, internalerror.NewBadRequestError("file must not be empty", nil)
	}
	if err := validateLineSpan(req.LineStart, req.LineEnd); err != nil {
		return nil, err
	}

	return nil, internalerror.NewPreconditionError(blameUnbuilt, nil)
}

// A zero end is the single line start, not a span ending before it begins.
func validateLineSpan(start, end int) error {
	if start < 1 {
		return internalerror.NewBadRequestError(
			fmt.Sprintf("line_start %d must be at least 1", start), nil)
	}
	if end != 0 && end < start {
		return internalerror.NewBadRequestError(
			fmt.Sprintf("line span %d-%d ends before it starts", start, end), nil)
	}

	return nil
}

func (w *whyService) validateRepo(requested string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if len(w.repos) == 1 {
			return nil
		}

		return internalerror.NewBadRequestError(
			"repo must name one of the registered repos: "+w.registeredRepos(), nil)
	}

	for _, repo := range w.repos {
		if requested == repo.Remote || requested == repo.Path {
			return nil
		}
	}

	return internalerror.NewNotFoundError(
		fmt.Sprintf("repo %q is not registered — registered repos: %s", requested, w.registeredRepos()), nil)
}

func (w *whyService) registeredRepos() string {
	names := make([]string, 0, len(w.repos))
	for _, repo := range w.repos {
		names = append(names, repo.name())
	}

	return strings.Join(names, ", ")
}

func (r CodeRepo) name() string {
	if r.Remote != "" {
		return r.Remote
	}

	return r.Path
}
