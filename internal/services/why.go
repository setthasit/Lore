package services

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"lore/internal/connectors/embedder"
	"lore/internal/connectors/gitrepo"
	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/repositories"
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
	Git    gitrepo.GitRepo
}

const askOnlyRefusal = "no repositories registered — code anchoring disabled for this workspace"

// The width the index stores commit SHAs at, so a shortened SHA still resolves.
const shortSHAChars = 12

type whyService struct {
	store repositories.IndexStore
	emb   embedder.Embedder
	cfg   QueryConfig
	repos []CodeRepo
}

var _ WhyService = (*whyService)(nil)

func NewWhyService(
	store repositories.IndexStore,
	emb embedder.Embedder,
	cfg QueryConfig,
	repos []CodeRepo,
) WhyService {
	if cfg.TopK <= 0 {
		cfg.TopK = defaultTopK
	}
	if cfg.WalkDepth <= 0 {
		cfg.WalkDepth = defaultWalkDepth
	}

	return &whyService{store: store, emb: emb, cfg: cfg, repos: slices.Clone(repos)}
}

func (w *whyService) Why(ctx context.Context, req WhyRequest) (*entities.EvidenceBundle, error) {
	if len(w.repos) == 0 {
		return nil, internalerror.NewPreconditionError(askOnlyRefusal, nil)
	}
	repo, err := w.matchRepo(req.Repo)
	if err != nil {
		return nil, err
	}
	file := strings.TrimSpace(req.File)
	if file == "" {
		return nil, internalerror.NewBadRequestError("file must not be empty", nil)
	}
	if err := validateLineSpan(req.LineStart, req.LineEnd); err != nil {
		return nil, err
	}
	span := codeSpan{repo: repo, file: file, start: req.LineStart, end: max(req.LineEnd, req.LineStart)}

	blamed, err := span.blame(ctx)
	if err != nil {
		return nil, err
	}
	unsynced, err := w.resolveBlamed(ctx, blamed)
	if err != nil {
		return nil, err
	}

	walked, err := walkGraph(ctx, w.store, blamedSeeds(blamed),
		walkOptions{Depth: w.cfg.WalkDepth, Direction: entities.DirBoth})
	if err != nil {
		return nil, internalerror.NewInternalError("walking the provenance graph failed", err)
	}

	question := whyQuestionOf(req.Question, span)
	matches, err := w.whyMatches(ctx, whyRetrievalText(question, blamed))
	if err != nil {
		return nil, err
	}

	nodes := whyNodes(blamed, walked, matches)
	chains := assembleChains(walked.Paths, walked.SeedLinks, nodes)

	return &entities.EvidenceBundle{
		Question: question,
		Anchor:   span.evidenceAnchor(blamedSHAs(blamed)),
		Nodes:    nodes,
		Chains:   chains,
		Gaps:     append(unsynced, standaloneSeedGaps(nodes, chains)...),
	}, nil
}

type codeSpan struct {
	repo  CodeRepo
	file  string
	start int
	end   int
}

func (s codeSpan) String() string {
	return fmt.Sprintf("%s:%d-%d", s.file, s.start, s.end)
}

func (s codeSpan) evidenceAnchor(shas []string) entities.Anchor {
	return entities.Anchor{
		Kind: entities.AnchorCodeSpan,
		Code: &entities.CodeAnchor{
			Repo:       s.repo.name(),
			File:       s.file,
			LineStart:  s.start,
			LineEnd:    s.end,
			BlamedSHAs: shas,
		},
	}
}

func (s codeSpan) blame(ctx context.Context) ([]blamedCommit, error) {
	tracked, err := s.repo.Git.HasFileAtHEAD(ctx, s.file)
	if err != nil {
		return nil, internalerror.NewInternalError(
			fmt.Sprintf("looking up %s in %s failed", s.file, s.repo.name()), err)
	}
	if !tracked {
		return nil, internalerror.NewNotFoundError(
			fmt.Sprintf("%s is not tracked at HEAD of %s", s.file, s.repo.name()), nil)
	}

	spans, err := s.repo.Git.Blame(ctx, s.file, s.start, s.end)
	if err != nil {
		return nil, internalerror.NewInternalError(fmt.Sprintf("blaming %s failed", s), err)
	}

	return blamedCommits(spans), nil
}

type blamedCommit struct {
	sha   string
	lines []string
	docs  []entities.DocumentMeta
}

func (c blamedCommit) excerpt() string {
	return anchorExcerpt(strings.Join(c.lines, "\n"))
}

func (c blamedCommit) short() string {
	if len(c.sha) <= shortSHAChars {
		return c.sha
	}

	return c.sha[:shortSHAChars]
}

// One commit per blamed SHA, in first-blamed order, carrying every line it owns.
func blamedCommits(spans []gitrepo.BlameSpan) []blamedCommit {
	commits := make([]blamedCommit, 0, len(spans))
	at := make(map[string]int, len(spans))
	for _, span := range spans {
		i, seen := at[span.SHA]
		if !seen {
			i = len(commits)
			at[span.SHA] = i
			commits = append(commits, blamedCommit{sha: span.SHA})
		}
		commits[i].lines = append(commits[i].lines, span.Lines...)
	}

	return commits
}

// A SHA the index never ingested is a gap, not a failure: blame still names it.
func (w *whyService) resolveBlamed(ctx context.Context, commits []blamedCommit) ([]string, error) {
	var unsynced []string
	for i := range commits {
		candidates, err := w.store.ResolveRef(ctx, commits[i].sha)
		if err != nil {
			return nil, internalerror.NewInternalError(
				fmt.Sprintf("resolving the blamed commit %s failed", commits[i].short()), err)
		}
		for _, candidate := range candidates {
			if candidate.Type == entities.DocTypeCommit {
				commits[i].docs = append(commits[i].docs, candidate)
			}
		}
		if len(commits[i].docs) == 0 {
			unsynced = append(unsynced,
				"trail ends at commit "+commits[i].short()+", not synced from a source")
		}
	}

	return unsynced, nil
}

func blamedSHAs(commits []blamedCommit) []string {
	shas := make([]string, len(commits))
	for i, commit := range commits {
		shas[i] = commit.sha
	}

	return shas
}

func blamedSeeds(commits []blamedCommit) []entities.DocID {
	seeds := make([]entities.DocID, 0, len(commits))
	for _, commit := range commits {
		for _, doc := range commit.docs {
			seeds = append(seeds, doc.ID)
		}
	}

	return seeds
}

func (w *whyService) whyMatches(ctx context.Context, text string) ([]seedHit, error) {
	fused, err := hybridSearch(ctx, w.store, w.emb, text, entities.Filters{}, w.cfg.TopK)
	if err != nil {
		return nil, err
	}

	return liftDocuments(ctx, w.store, fused)
}

func whyQuestionOf(question string, span codeSpan) string {
	if asked := strings.TrimSpace(question); asked != "" {
		return asked
	}

	return "why does " + span.String() + " exist in its current form"
}

func whyRetrievalText(question string, commits []blamedCommit) string {
	parts := []string{question}
	if code := anchorExcerpt(blamedCode(commits)); code != "" {
		parts = append(parts, code)
	}
	if subjects := blamedSubjects(commits); subjects != "" {
		parts = append(parts, subjects)
	}

	return strings.Join(parts, "\n\n")
}

func blamedCode(commits []blamedCommit) string {
	lines := make([]string, 0, len(commits))
	for _, commit := range commits {
		lines = append(lines, commit.lines...)
	}

	return strings.Join(lines, "\n")
}

func blamedSubjects(commits []blamedCommit) string {
	subjects := make([]string, 0, len(commits))
	for _, commit := range commits {
		for _, doc := range commit.docs {
			if doc.Title != "" {
				subjects = append(subjects, doc.Title)
			}
		}
	}

	return strings.Join(subjects, "\n")
}

func whyNodes(commits []blamedCommit, walked walkResult, matches []seedHit) []entities.EvidenceNode {
	collected := newNodeSet(len(commits) + len(walked.Paths) + len(matches))
	for _, commit := range commits {
		excerpt := commit.excerpt()
		for _, doc := range commit.docs {
			collected.add(entities.EvidenceNode{
				Doc:     doc,
				Excerpt: excerpt,
				Role:    entities.RoleBlamedCommit,
				Score:   1,
			})
		}
	}
	collected.addWalked(walked, graphRole)
	collected.addMatches(matches)
	slices.SortFunc(collected.nodes, byRank)

	return collected.nodes
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

func (w *whyService) matchRepo(requested string) (CodeRepo, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if len(w.repos) == 1 {
			return w.repos[0], nil
		}

		return CodeRepo{}, internalerror.NewBadRequestError(
			"repo must name one of the registered repos: "+w.registeredRepos(), nil)
	}

	for _, repo := range w.repos {
		if requested == repo.Remote || requested == repo.Path {
			return repo, nil
		}
	}

	return CodeRepo{}, internalerror.NewNotFoundError(
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
