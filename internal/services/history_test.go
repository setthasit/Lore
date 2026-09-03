package services_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/connectors/gitrepo"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	mock_gitrepo "github.com/setthasit/Lore/internal/mocks/gitrepo"
	mock_repositories "github.com/setthasit/Lore/internal/mocks/repositories"
	"github.com/setthasit/Lore/internal/services"
)

const histFile = "internal/auth/session.go"

const (
	histDefaultLimit = 20
	histMaxLimit     = 50
)

const (
	histShaOld = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	histShaMid = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	histShaNew = "cccccccccccccccccccccccccccccccccccccccc"
)

const (
	histShaTwinA = "ab11111111111111111111111111111111111111"
	histShaTwinB = "ab22222222222222222222222222222222222222"
)

const (
	histOldID   entities.DocID = "github:commit:old"
	histMidID   entities.DocID = "github:commit:mid"
	histNewID   entities.DocID = "github:commit:new"
	histPRID    entities.DocID = "github:pr:88"
	histIssueID entities.DocID = "github:issue:12"
)

var (
	errHistGit   = errors.New("clone is unreadable")
	errHistStore = errors.New("index is unavailable")
)

var histAt = time.Date(2025, 6, 4, 11, 0, 0, 0, time.UTC)

var (
	histOldMeta   = whyMeta(histOldID, entities.DocTypeCommit, "introduce the session store", histAt)
	histMidMeta   = whyMeta(histMidID, entities.DocTypeCommit, "expire idle sessions", histAt.Add(whyDay))
	histNewMeta   = whyMeta(histNewID, entities.DocTypeCommit, "rename session.go", histAt.Add(2*whyDay))
	histPRMeta    = whyMeta(histPRID, entities.DocTypePR, "expire idle sessions", histAt.Add(3*whyDay))
	histIssueMeta = whyMeta(histIssueID, entities.DocTypeIssue, "sessions never expire", histAt.Add(-5*whyDay))
)

var (
	histPRTouchesMid    = whyEdge(histPRID, histMidID)
	histIssueTouchesOld = whyEdge(histIssueID, histOldID)
)

// git log --follow answers newest first, which is the order the cursor indexes.
var histLog = []gitrepo.CommitRef{
	histCommit(histShaNew, histAt.Add(2*whyDay)),
	histCommit(histShaMid, histAt.Add(whyDay)),
	histCommit(histShaOld, histAt),
}

func histCommit(sha string, at time.Time) gitrepo.CommitRef {
	return gitrepo.CommitRef{SHA: sha, Author: "Ada Lovelace", Time: at, Subject: "touching " + histFile}
}

func histSyntheticLog(n int) []gitrepo.CommitRef {
	log := make([]gitrepo.CommitRef, n)
	for i := range log {
		log[i] = histCommit(fmt.Sprintf("%040x", i), histAt.Add(-time.Duration(i)*whyDay))
	}

	return log
}

func histSHAs(commits []gitrepo.CommitRef) []string {
	shas := make([]string, len(commits))
	for i, commit := range commits {
		shas[i] = commit.SHA
	}

	return shas
}

func histUnsyncedGaps(commits []gitrepo.CommitRef) []string {
	gaps := make([]string, len(commits))
	for i, commit := range commits {
		gaps[i] = whyUnsyncedGap(commit.SHA)
	}

	return gaps
}

func histRequest() services.HistoryRequest {
	return services.HistoryRequest{File: histFile}
}

type histFixture struct {
	store *mock_repositories.MockIndexStore
	git   *mock_gitrepo.MockGitRepo
	svc   services.HistoryService
}

func newHistFixture(t *testing.T) histFixture {
	t.Helper()

	ctrl := gomock.NewController(t)
	store := mock_repositories.NewMockIndexStore(ctrl)
	git := mock_gitrepo.NewMockGitRepo(ctrl)
	repos := []services.CodeRepo{{Path: whyPath, Remote: whyRemote, Git: git}}

	return histFixture{store: store, git: git, svc: services.NewHistoryService(store, repos)}
}

func (f histFixture) expectLog(commits ...gitrepo.CommitRef) {
	f.git.EXPECT().HasFileAtHEAD(gomock.Any(), histFile).Return(true, nil)
	f.git.EXPECT().Log(gomock.Any(), histFile).Return(commits, nil)
}

func (f histFixture) expectResolve(sha string, candidates ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().ResolveRef(gomock.Any(), sha).Return(candidates, nil)
}

func (f histFixture) expectMetas(ids []entities.DocID, metas ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().DocumentsByID(gomock.Any(), ids).Return(metas, nil)
}

func (f histFixture) expectNeighbors(ids []entities.DocID, edges ...entities.Edge) *gomock.Call {
	return f.store.EXPECT().Neighbors(gomock.Any(), ids, nil, entities.DirBoth).Return(edges, nil)
}

// Depth 1: the seed layer is asked for neighbours exactly once, and never again.
func (f histFixture) expectOneHop(seeds []entities.DocID, edges []entities.Edge, reached ...entities.DocumentMeta) {
	f.expectNeighbors(seeds, edges...)
	f.expectMetas(metaIDsOf(reached), reached...)
}

func metaIDsOf(metas []entities.DocumentMeta) []entities.DocID {
	ids := make([]entities.DocID, len(metas))
	for i, meta := range metas {
		ids[i] = meta.ID
	}

	return ids
}

func assertHistAnchor(t *testing.T, anchor entities.Anchor, shas []string) {
	t.Helper()

	if anchor.Kind != entities.AnchorCodeSpan {
		t.Errorf("Anchor.Kind = %d, want %d", anchor.Kind, entities.AnchorCodeSpan)
	}
	if anchor.Doc != nil || anchor.Window != nil || anchor.Query != "" {
		t.Errorf("Anchor carries a non-code grounding: %+v", anchor)
	}
	if anchor.Code == nil {
		t.Fatalf("Anchor.Code = nil, want the anchored file")
	}
	if anchor.Code.Repo != whyRemote || anchor.Code.File != histFile {
		t.Errorf("Anchor.Code = %s %s, want %s %s",
			anchor.Code.Repo, anchor.Code.File, whyRemote, histFile)
	}
	if anchor.Code.LineStart != 0 || anchor.Code.LineEnd != 0 {
		t.Errorf("Anchor.Code span = %d-%d, want a whole-file anchor with no line span",
			anchor.Code.LineStart, anchor.Code.LineEnd)
	}
	if !slices.Equal(anchor.Code.BlamedSHAs, shas) {
		t.Errorf("Anchor.Code.BlamedSHAs = %v, want %v", anchor.Code.BlamedSHAs, shas)
	}
}

func TestHistoryOfTimelinesEveryCommitWithItsLinkedLayer(t *testing.T) {
	t.Parallel()

	f := newHistFixture(t)
	f.expectLog(histLog...)
	f.expectResolve(histShaNew, histNewMeta)
	f.expectResolve(histShaMid, histMidMeta)
	f.expectResolve(histShaOld, histOldMeta)

	seeds := []entities.DocID{histNewID, histMidID, histOldID}
	f.expectMetas(seeds, histNewMeta, histMidMeta, histOldMeta)
	f.expectOneHop(seeds,
		[]entities.Edge{histPRTouchesMid, histIssueTouchesOld},
		histPRMeta, histIssueMeta)

	bundle, err := f.svc.HistoryOf(context.Background(), histRequest())
	if err != nil {
		t.Fatalf("HistoryOf: %v", err)
	}

	if got := nodeIDs(bundle.Nodes); !slices.Equal(got, []entities.DocID{
		histIssueID, histOldID, histMidID, histNewID, histPRID,
	}) {
		t.Fatalf("Nodes = %v, want them oldest first", got)
	}
	if got := whyRoles(bundle.Nodes); !slices.Equal(got, []string{
		entities.RoleLinkedTicket,
		entities.RoleBlamedCommit,
		entities.RoleBlamedCommit,
		entities.RoleBlamedCommit,
		entities.RoleLinkedChange,
	}) {
		t.Errorf("roles = %q, want the commits blamed and their layer linked", got)
	}
	assertScore(t, "windowed commit", bundle.Nodes[1].Score, 1)
	assertScore(t, "linked change", bundle.Nodes[4].Score, 0.6)
	if !slices.Equal(bundle.Nodes[4].Via, []entities.Edge{histPRTouchesMid}) {
		t.Errorf("linked change Via = %+v, want %+v", bundle.Nodes[4].Via, histPRTouchesMid)
	}

	if bundle.Question != "history of "+histFile+" in "+whyRemote {
		t.Errorf("Question = %q, want it to name the file and the repo", bundle.Question)
	}
	assertHistAnchor(t, bundle.Anchor, []string{histShaNew, histShaMid, histShaOld})
	assertWhyChains(t, bundle.Chains, [][]entities.DocID{
		{histMidID, histPRID},
		{histOldID, histIssueID},
	})
	assertGaps(t, bundle.Gaps, []string{
		histNewMeta.Title + " (" + string(histNewID) + ") stands alone; no linked discussion",
	})
}

func TestHistoryOfReportsAnUnsyncedCommitAsAGapAndKeepsTheRest(t *testing.T) {
	t.Parallel()

	f := newHistFixture(t)
	f.expectLog(histLog...)
	f.expectResolve(histShaNew)
	// A non-commit candidate is not the commit, so this SHA still ends the trail.
	f.expectResolve(histShaMid, histPRMeta)
	f.expectResolve(histShaOld, histOldMeta)

	seeds := []entities.DocID{histOldID}
	f.expectMetas(seeds, histOldMeta)
	f.expectOneHop(seeds, []entities.Edge{histIssueTouchesOld}, histIssueMeta)

	bundle, err := f.svc.HistoryOf(context.Background(), histRequest())
	if err != nil {
		t.Fatalf("HistoryOf: %v", err)
	}

	if got := nodeIDs(bundle.Nodes); !slices.Equal(got, []entities.DocID{histIssueID, histOldID}) {
		t.Errorf("Nodes = %v, want only the synced commit and its ticket", got)
	}
	assertGaps(t, bundle.Gaps, []string{whyUnsyncedGap(histShaNew), whyUnsyncedGap(histShaMid)})
	assertHistAnchor(t, bundle.Anchor, []string{histShaNew, histShaMid, histShaOld})
}

func TestHistoryOfBoundsTheWindowServerSide(t *testing.T) {
	t.Parallel()

	log := histSyntheticLog(2 * histMaxLimit)

	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "an omitted limit takes the default", limit: 0, want: histDefaultLimit},
		{name: "a negative limit takes the default", limit: -4, want: histDefaultLimit},
		{name: "a smaller limit is honoured", limit: 3, want: 3},
		{name: "the maximum is reachable", limit: histMaxLimit, want: histMaxLimit},
		{name: "an oversized limit is clamped, never refused", limit: 5000, want: histMaxLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newHistFixture(t)
			f.expectLog(log...)
			f.store.EXPECT().ResolveRef(gomock.Any(), gomock.Any()).Return(nil, nil).Times(tt.want)

			bundle, err := f.svc.HistoryOf(context.Background(),
				services.HistoryRequest{File: histFile, Limit: tt.limit})
			if err != nil {
				t.Fatalf("HistoryOf: %v", err)
			}

			assertHistAnchor(t, bundle.Anchor, histSHAs(log[:tt.want]))
			assertGaps(t, bundle.Gaps, histUnsyncedGaps(log[:tt.want]))
		})
	}
}

func TestHistoryOfPagesTheCommitsOlderThanTheCursor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		before string
	}{
		{name: "a full SHA", before: histShaNew},
		{name: "an unambiguous abbreviation", before: histShaNew[:7]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newHistFixture(t)
			f.expectLog(histLog...)
			f.expectResolve(histShaMid, histMidMeta)
			f.expectResolve(histShaOld, histOldMeta)

			seeds := []entities.DocID{histMidID, histOldID}
			f.expectMetas(seeds, histMidMeta, histOldMeta)
			f.expectOneHop(seeds,
				[]entities.Edge{histPRTouchesMid, histIssueTouchesOld},
				histPRMeta, histIssueMeta)

			bundle, err := f.svc.HistoryOf(context.Background(),
				services.HistoryRequest{File: histFile, Before: tt.before})
			if err != nil {
				t.Fatalf("HistoryOf: %v", err)
			}

			if got := nodeIDs(bundle.Nodes); !slices.Equal(got, []entities.DocID{
				histIssueID, histOldID, histMidID, histPRID,
			}) {
				t.Errorf("Nodes = %v, want the window before %s", got, tt.before)
			}
			assertHistAnchor(t, bundle.Anchor, []string{histShaMid, histShaOld})
		})
	}
}

// The bundle must carry its own next cursor: the oldest SHA of the window, which
// Anchor.Code.BlamedSHAs keeps last because the log runs newest first.
func TestHistoryOfPagesContiguouslyFromTheAnchorSHAs(t *testing.T) {
	t.Parallel()

	f := newHistFixture(t)
	f.git.EXPECT().HasFileAtHEAD(gomock.Any(), histFile).Return(true, nil).Times(4)
	f.git.EXPECT().Log(gomock.Any(), histFile).Return(histLog, nil).Times(4)
	f.expectResolve(histShaNew)
	f.expectResolve(histShaMid)
	f.expectResolve(histShaOld)

	var walked []string
	before := ""
	for range 4 {
		bundle, err := f.svc.HistoryOf(context.Background(),
			services.HistoryRequest{File: histFile, Limit: 1, Before: before})
		if err != nil {
			t.Fatalf("HistoryOf before %q: %v", before, err)
		}
		shas := bundle.Anchor.Code.BlamedSHAs
		if len(shas) == 0 {
			break
		}
		walked = append(walked, shas...)
		before = shas[len(shas)-1]
	}

	if !slices.Equal(walked, []string{histShaNew, histShaMid, histShaOld}) {
		t.Errorf("paged SHAs = %v, want the whole log newest first, once each", walked)
	}
	if before != histShaOld {
		t.Errorf("paging stopped at %q, want it to terminate past the oldest commit", before)
	}
}

func TestHistoryOfEndsThePagingAtTheOldestCommit(t *testing.T) {
	t.Parallel()

	f := newHistFixture(t)
	f.expectLog(histLog...)

	bundle, err := f.svc.HistoryOf(context.Background(),
		services.HistoryRequest{File: histFile, Before: histShaOld})
	if err != nil {
		t.Fatalf("HistoryOf past the oldest commit: %v", err)
	}

	if len(bundle.Nodes) != 0 {
		t.Errorf("Nodes = %v, want none older than the first commit", nodeIDs(bundle.Nodes))
	}
	assertHistAnchor(t, bundle.Anchor, nil)
}

func TestHistoryOfRefusesACursorTheLogDoesNotCarry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		log      []gitrepo.CommitRef
		before   string
		kind     internalerror.Kind
		contains []string
	}{
		{
			name:     "a SHA absent from the log",
			log:      histLog,
			before:   "deadbeefdeadbeef",
			kind:     internalerror.KindNotFound,
			contains: []string{"deadbeefdeadbeef", histFile, whyRemote},
		},
		{
			name:     "an abbreviation matching several commits",
			log:      []gitrepo.CommitRef{histCommit(histShaTwinA, histAt), histCommit(histShaTwinB, histAt)},
			before:   "ab",
			kind:     internalerror.KindBadRequest,
			contains: []string{`"ab"`, histShaTwinA[:12], histShaTwinB[:12], "full SHA"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newHistFixture(t)
			f.expectLog(tt.log...)

			bundle, err := f.svc.HistoryOf(context.Background(),
				services.HistoryRequest{File: histFile, Before: tt.before})

			if bundle != nil {
				t.Errorf("bundle = %+v, want none alongside a refusal", bundle)
			}
			if got := internalerror.KindOf(err); got != tt.kind {
				t.Fatalf("kind = %s, want %s (error %v)", got, tt.kind, err)
			}
			for _, want := range tt.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message = %q, want it to name %q", err.Error(), want)
				}
			}
		})
	}
}

func TestHistoryOfReturnsACoherentBundleForAFileWithNoHistory(t *testing.T) {
	t.Parallel()

	f := newHistFixture(t)
	f.expectLog()

	bundle, err := f.svc.HistoryOf(context.Background(), histRequest())
	if err != nil {
		t.Fatalf("HistoryOf on an empty history: %v", err)
	}

	if len(bundle.Nodes) != 0 {
		t.Errorf("Nodes = %v, want none", nodeIDs(bundle.Nodes))
	}
	if bundle.Chains != nil {
		t.Errorf("Chains = %v, want none", bundle.Chains)
	}
	assertGaps(t, bundle.Gaps, nil)
	if bundle.Question != "history of "+histFile+" in "+whyRemote {
		t.Errorf("Question = %q, want it to name the file and the repo", bundle.Question)
	}
	assertHistAnchor(t, bundle.Anchor, nil)
}

func TestHistoryOfRefusesAFileAbsentAtHEADWithoutReadingTheLog(t *testing.T) {
	t.Parallel()

	// Only HasFileAtHEAD is expected: an absent file has no history to read.
	f := newHistFixture(t)
	f.git.EXPECT().HasFileAtHEAD(gomock.Any(), histFile).Return(false, nil)

	bundle, err := f.svc.HistoryOf(context.Background(), histRequest())

	if bundle != nil {
		t.Errorf("bundle = %+v, want none for an absent file", bundle)
	}
	if got := internalerror.KindOf(err); got != internalerror.KindNotFound {
		t.Fatalf("kind = %s, want %s (error %v)", got, internalerror.KindNotFound, err)
	}
	for _, want := range []string{histFile, whyRemote} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message = %q, want it to name %q", err.Error(), want)
		}
	}
}

func TestHistoryOfValidationOrder(t *testing.T) {
	tests := []struct {
		name     string
		repos    []services.CodeRepo
		req      services.HistoryRequest
		kind     internalerror.Kind
		message  string
		contains []string
	}{
		{
			name:    "no repositories registered refuses code anchoring",
			req:     services.HistoryRequest{File: histFile},
			kind:    internalerror.KindPrecondition,
			message: askOnlyMessage,
		},
		{
			name:    "no repositories outranks every other complaint",
			req:     services.HistoryRequest{Repo: "github:acme/other", File: "  "},
			kind:    internalerror.KindPrecondition,
			message: askOnlyMessage,
		},
		{
			name:     "an unregistered repo names the request and the registered repos",
			repos:    twoRepos,
			req:      services.HistoryRequest{Repo: "github:acme/other", File: histFile},
			kind:     internalerror.KindNotFound,
			contains: []string{"github:acme/other", whyRemote, otherPath},
		},
		{
			name:     "an unregistered repo outranks a blank file",
			repos:    twoRepos,
			req:      services.HistoryRequest{Repo: "github:acme/other", File: "  "},
			kind:     internalerror.KindNotFound,
			contains: []string{"github:acme/other"},
		},
		{
			name:     "an omitted repo with several registered asks for one",
			repos:    twoRepos,
			req:      services.HistoryRequest{File: histFile},
			kind:     internalerror.KindBadRequest,
			contains: []string{"repo must name one", whyRemote, otherPath},
		},
		{
			name:     "a blank file is rejected before the clone is touched",
			repos:    oneRepo,
			req:      services.HistoryRequest{File: "   ", Before: histShaNew, Limit: 9},
			kind:     internalerror.KindBadRequest,
			contains: []string{"file"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Every repo here carries a nil Git: none of these refusals may reach a clone.
			svc := services.NewHistoryService(nil, tt.repos)

			bundle, err := svc.HistoryOf(context.Background(), tt.req)

			if bundle != nil {
				t.Errorf("bundle = %+v, want none alongside a refusal", bundle)
			}
			if got := internalerror.KindOf(err); got != tt.kind {
				t.Fatalf("kind = %s, want %s (error %v)", got, tt.kind, err)
			}
			if tt.message != "" && err.Error() != tt.message {
				t.Errorf("message = %q, want %q", err.Error(), tt.message)
			}
			for _, want := range tt.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message = %q, want it to name %q", err.Error(), want)
				}
			}
		})
	}
}

func TestHistoryOfSurfacesGitAndStoreFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		arrange  func(f histFixture)
		cause    error
		contains []string
	}{
		{
			name: "an unreadable clone",
			arrange: func(f histFixture) {
				f.git.EXPECT().HasFileAtHEAD(gomock.Any(), histFile).Return(false, errHistGit)
			},
			cause:    errHistGit,
			contains: []string{histFile, whyRemote},
		},
		{
			name: "an unreadable log",
			arrange: func(f histFixture) {
				f.git.EXPECT().HasFileAtHEAD(gomock.Any(), histFile).Return(true, nil)
				f.git.EXPECT().Log(gomock.Any(), histFile).Return(nil, errHistGit)
			},
			cause:    errHistGit,
			contains: []string{histFile, whyRemote},
		},
		{
			name: "an unresolvable commit",
			arrange: func(f histFixture) {
				f.expectLog(histLog...)
				f.store.EXPECT().ResolveRef(gomock.Any(), histShaNew).Return(nil, errHistStore)
			},
			cause:    errHistStore,
			contains: []string{histShaNew[:12]},
		},
		{
			name: "an unwalkable graph",
			arrange: func(f histFixture) {
				f.expectLog(histLog[2])
				f.expectResolve(histShaOld, histOldMeta)
				f.expectMetas([]entities.DocID{histOldID}, histOldMeta)
				f.store.EXPECT().
					Neighbors(gomock.Any(), []entities.DocID{histOldID}, nil, entities.DirBoth).
					Return(nil, errHistStore)
			},
			cause:    errHistStore,
			contains: []string{"provenance graph"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newHistFixture(t)
			tt.arrange(f)

			bundle, err := f.svc.HistoryOf(context.Background(), histRequest())

			if bundle != nil {
				t.Errorf("bundle = %+v, want none alongside a failure", bundle)
			}
			if got := internalerror.KindOf(err); got != internalerror.KindInternal {
				t.Fatalf("kind = %s, want %s (error %v)", got, internalerror.KindInternal, err)
			}
			if !errors.Is(err, tt.cause) {
				t.Errorf("error %v does not wrap %v", err, tt.cause)
			}
			for _, want := range tt.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message = %q, want it to name %q", err.Error(), want)
				}
			}
		})
	}
}
