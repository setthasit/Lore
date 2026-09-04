package services

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/repositories"
	"github.com/setthasit/Lore/sdk"
)

type HistoryService interface {
	// Before is a commit SHA, abbreviated or full; the window holds the commits older than it.
	HistoryOf(ctx context.Context, req HistoryRequest) (*entities.EvidenceBundle, error)
}

type HistoryRequest struct {
	Repo   string
	File   string
	Limit  int
	Before string
}

const (
	defaultHistoryLimit = 20
	maxHistoryLimit     = 50
)

const historyWalkDepth = 1

type historyService struct {
	store repositories.IndexStore
	repos []CodeRepo
}

var _ HistoryService = (*historyService)(nil)

func NewHistoryService(store repositories.IndexStore, repos []CodeRepo) HistoryService {
	return &historyService{store: store, repos: slices.Clone(repos)}
}

func (h *historyService) HistoryOf(ctx context.Context, req HistoryRequest) (*entities.EvidenceBundle, error) {
	if len(h.repos) == 0 {
		return nil, internalerror.NewPreconditionError(askOnlyRefusal, nil)
	}
	repo, err := matchRepo(h.repos, req.Repo)
	if err != nil {
		return nil, err
	}
	file := strings.TrimSpace(req.File)
	if file == "" {
		return nil, internalerror.NewBadRequestError("file must not be empty", nil)
	}

	history, err := fileHistoryOf(ctx, repo, file)
	if err != nil {
		return nil, err
	}
	window, err := history.window(req.Before, historyLimit(req.Limit))
	if err != nil {
		return nil, err
	}

	commits, unsynced, err := h.resolveWindow(ctx, window)
	if err != nil {
		return nil, err
	}

	walked, err := walkGraph(ctx, h.store, metaIDs(commits),
		walkOptions{Depth: historyWalkDepth, Direction: entities.DirBoth})
	if err != nil {
		return nil, internalerror.NewInternalError("walking the provenance graph failed", err)
	}

	nodes := historyNodes(commits, walked)
	chains := assembleChains(walked.Paths, walked.SeedLinks, nodes)

	return &entities.EvidenceBundle{
		Question: history.question(),
		Anchor:   history.evidenceAnchor(windowSHAs(window)),
		Nodes:    nodes,
		Chains:   chains,
		Gaps:     append(unsynced, standaloneSeedGaps(nodes, chains)...),
	}, nil
}

type fileHistory struct {
	repo CodeRepo
	file string
	log  []lore.CommitRef
}

func fileHistoryOf(ctx context.Context, repo CodeRepo, file string) (fileHistory, error) {
	if err := requireTrackedFile(ctx, repo, file); err != nil {
		return fileHistory{}, err
	}

	log, err := repo.Git.Log(ctx, file)
	if err != nil {
		return fileHistory{}, internalerror.NewInternalError(
			fmt.Sprintf("reading the history of %s in %s failed", file, repo.name()), err)
	}

	return fileHistory{repo: repo, file: file, log: log}, nil
}

func (h fileHistory) window(before string, limit int) ([]lore.CommitRef, error) {
	from := 0
	if cursor := strings.TrimSpace(before); cursor != "" {
		at, err := h.cursorAt(cursor)
		if err != nil {
			return nil, err
		}
		from = at + 1
	}
	if from >= len(h.log) {
		return nil, nil
	}

	return h.log[from:min(from+limit, len(h.log))], nil
}

func (h fileHistory) cursorAt(cursor string) (int, error) {
	at := -1
	for i, commit := range h.log {
		if !strings.HasPrefix(commit.SHA, cursor) {
			continue
		}
		if at >= 0 {
			return 0, internalerror.NewBadRequestError(fmt.Sprintf(
				"before %q matches both %s and %s — retry with a full SHA",
				cursor, shortSHA(h.log[at].SHA), shortSHA(commit.SHA)), nil)
		}
		at = i
	}
	if at < 0 {
		return 0, internalerror.NewNotFoundError(fmt.Sprintf(
			"before %q names no commit in the history of %s in %s", cursor, h.file, h.repo.name()), nil)
	}

	return at, nil
}

// history_of anchors on a whole file, so the 1-based line span stays zero.
func (h fileHistory) evidenceAnchor(shas []string) entities.Anchor {
	return entities.Anchor{
		Kind: entities.AnchorCodeSpan,
		Code: &entities.CodeAnchor{
			Repo:       h.repo.name(),
			File:       h.file,
			BlamedSHAs: shas,
		},
	}
}

func (h fileHistory) question() string {
	return "history of " + h.file + " in " + h.repo.name()
}

func historyLimit(limit int) int {
	if limit <= 0 {
		return defaultHistoryLimit
	}

	return min(limit, maxHistoryLimit)
}

func windowSHAs(window []lore.CommitRef) []string {
	shas := make([]string, len(window))
	for i, commit := range window {
		shas[i] = commit.SHA
	}

	return shas
}

func (h *historyService) resolveWindow(
	ctx context.Context,
	window []lore.CommitRef,
) ([]entities.DocumentMeta, []string, error) {
	commits := make([]entities.DocumentMeta, 0, len(window))
	var unsynced []string
	for _, ref := range window {
		resolved, err := indexedCommits(ctx, h.store, ref.SHA)
		if err != nil {
			return nil, nil, internalerror.NewInternalError(
				fmt.Sprintf("resolving the commit %s failed", shortSHA(ref.SHA)), err)
		}
		if len(resolved) == 0 {
			unsynced = append(unsynced, unsyncedCommitGap(ref.SHA))
			continue
		}
		commits = append(commits, resolved...)
	}

	return commits, unsynced, nil
}

func metaIDs(metas []entities.DocumentMeta) []lore.DocID {
	ids := make([]lore.DocID, len(metas))
	for i, meta := range metas {
		ids[i] = meta.ID
	}

	return ids
}

func historyNodes(commits []entities.DocumentMeta, walked walkResult) []entities.EvidenceNode {
	collected := newNodeSet(len(commits) + len(walked.Paths))
	for _, commit := range commits {
		collected.add(entities.EvidenceNode{Doc: commit, Role: entities.RoleBlamedCommit, Score: 1})
	}
	collected.addWalked(walked, graphRole)
	slices.SortFunc(collected.nodes, byChronology)

	return collected.nodes
}
