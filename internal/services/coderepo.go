package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

const askOnlyRefusal = "no repositories registered — code anchoring disabled for this workspace"

// The width the index stores commit SHAs at, so a shortened SHA still resolves.
const shortSHAChars = 12

// Remote is the "github:acme/lore" name mapping the clone onto a source repo.
type CodeRepo struct {
	Path   string
	Remote string
	Git    lore.CodeRepo
}

func (r CodeRepo) name() string {
	if r.Remote != "" {
		return r.Remote
	}

	return r.Path
}

func matchRepo(repos []CodeRepo, requested string) (CodeRepo, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if len(repos) == 1 {
			return repos[0], nil
		}

		return CodeRepo{}, internalerror.NewBadRequestError(
			"repo must name one of the registered repos: "+registeredRepos(repos), nil)
	}

	for _, repo := range repos {
		if requested == repo.Remote || requested == repo.Path {
			return repo, nil
		}
	}

	return CodeRepo{}, internalerror.NewNotFoundError(
		fmt.Sprintf("repo %q is not registered — registered repos: %s", requested, registeredRepos(repos)), nil)
}

func registeredRepos(repos []CodeRepo) string {
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		names = append(names, repo.name())
	}

	return strings.Join(names, ", ")
}

func shortSHA(sha string) string {
	if len(sha) <= shortSHAChars {
		return sha
	}

	return sha[:shortSHAChars]
}

func unsyncedCommitGap(sha string) string {
	return "trail ends at commit " + shortSHA(sha) + ", not synced from a source"
}

func requireTrackedFile(ctx context.Context, repo CodeRepo, file string) error {
	tracked, err := repo.Git.HasFileAtHEAD(ctx, file)
	if err != nil {
		return internalerror.NewInternalError(
			fmt.Sprintf("looking up %s in %s failed", file, repo.name()), err)
	}
	if !tracked {
		return internalerror.NewNotFoundError(
			fmt.Sprintf("%s is not tracked at HEAD of %s", file, repo.name()), nil)
	}

	return nil
}

type commitSource interface {
	ResolveRef(ctx context.Context, ref string) ([]entities.DocumentMeta, error)
}

// A SHA the index never ingested resolves to nothing, which is a gap, not a failure.
func indexedCommits(ctx context.Context, s commitSource, sha string) ([]entities.DocumentMeta, error) {
	candidates, err := s.ResolveRef(ctx, sha)
	if err != nil {
		return nil, err
	}

	var commits []entities.DocumentMeta
	for _, candidate := range candidates {
		if candidate.Type == lore.DocTypeCommit {
			commits = append(commits, candidate)
		}
	}

	return commits, nil
}
