package gitrepo

import (
	"context"
	"strings"
	"testing"
)

const (
	addedAt  = "2024-03-04T09:00:00+02:00"
	editedAt = "2024-03-05T12:30:00Z"
)

// blameFixture leaves app.go attributed A,A,B,B,A,A: commit A appears, commit B
// interrupts it, and commit A returns without git repeating its author block.
func blameFixture(t *testing.T) (git *Repo, shaA, shaB string) {
	t.Helper()
	repo := newTestRepo(t)
	repo.write("app.go", "a1\na2\na3\na4\na5\na6\n")
	shaA = repo.commit(authorAda, addedAt, "add app")
	repo.write("app.go", "a1\na2\nb3\nb4\na5\na6\n")
	shaB = repo.commit(authorGrace, editedAt, "edit app")
	return repo.connector(), shaA, shaB
}

func assertSpans(t *testing.T, got, want []BlameSpan) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d spans, want %d\n got %+v\nwant %+v", len(got), len(want), got, want)
	}
	for i, w := range want {
		g := got[i]
		if g.SHA != w.SHA || g.LineStart != w.LineStart || g.LineEnd != w.LineEnd ||
			g.Author != w.Author || !g.Time.Equal(w.Time) {
			t.Errorf("span %d\n got %+v\nwant %+v", i, g, w)
		}
	}
}

func TestBlameCollapsesRunsAndReusesCachedMetadata(t *testing.T) {
	git, shaA, shaB := blameFixture(t)
	addedTime, editedTime := mustTime(t, addedAt), mustTime(t, editedAt)

	got, err := git.Blame(context.Background(), "app.go", 1, 6)
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	assertSpans(t, got, []BlameSpan{
		{SHA: shaA, LineStart: 1, LineEnd: 2, Author: authorAda, Time: addedTime},
		{SHA: shaB, LineStart: 3, LineEnd: 4, Author: authorGrace, Time: editedTime},
		{SHA: shaA, LineStart: 5, LineEnd: 6, Author: authorAda, Time: addedTime},
	})
}

func TestBlameSingleLine(t *testing.T) {
	git, _, shaB := blameFixture(t)

	got, err := git.Blame(context.Background(), "app.go", 3, 3)
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	assertSpans(t, got, []BlameSpan{
		{SHA: shaB, LineStart: 3, LineEnd: 3, Author: authorGrace, Time: mustTime(t, editedAt)},
	})
}

func TestBlameClampsEndBeyondLastLine(t *testing.T) {
	git, shaA, _ := blameFixture(t)

	got, err := git.Blame(context.Background(), "app.go", 5, 100)
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	assertSpans(t, got, []BlameSpan{
		{SHA: shaA, LineStart: 5, LineEnd: 6, Author: authorAda, Time: mustTime(t, addedAt)},
	})
}

func TestBlameRejectsStartBeyondLastLine(t *testing.T) {
	git, _, _ := blameFixture(t)

	got, err := git.Blame(context.Background(), "app.go", 20, 25)
	if err == nil {
		t.Fatalf("Blame past the end of the file = %v, want an error", got)
	}
	if !strings.Contains(err.Error(), "only 6 lines") {
		t.Errorf("error %q should carry git's stderr", err)
	}
}

func TestBlameRejectsImpossibleRangeBeforeRunningGit(t *testing.T) {
	git := New(t.TempDir())
	ctx := context.Background()

	for _, tt := range []struct {
		name             string
		start, end       int
		wantErrSubstring string
	}{
		{"start below one", 0, 3, "below 1"},
		{"end before start", 4, 2, "precedes start line"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := git.Blame(ctx, "app.go", tt.start, tt.end)
			if err == nil {
				t.Fatalf("Blame(%d,%d) = %v, want an error", tt.start, tt.end, got)
			}
			if !strings.Contains(err.Error(), tt.wantErrSubstring) {
				t.Errorf("error %q should mention %q", err, tt.wantErrSubstring)
			}
		})
	}
}

func TestBlamePinsHEADOverADirtyWorkingTree(t *testing.T) {
	repo := newTestRepo(t)
	repo.write("app.go", "a1\na2\na3\n")
	sha := repo.commit(authorAda, addedAt, "add app")
	repo.write("app.go", "a1\nuncommitted\na3\n")

	got, err := repo.connector().Blame(context.Background(), "app.go", 1, 3)
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	assertSpans(t, got, []BlameSpan{
		{SHA: sha, LineStart: 1, LineEnd: 3, Author: authorAda, Time: mustTime(t, addedAt)},
	})
}

func TestBlameMissingFile(t *testing.T) {
	git, _, _ := blameFixture(t)

	got, err := git.Blame(context.Background(), "nope.go", 1, 1)
	if err == nil {
		t.Fatalf("Blame of an absent path = %v, want an error", got)
	}
}

func TestBlameParsesSHA256ObjectNames(t *testing.T) {
	repo := newSHA256TestRepo(t)
	repo.write("app.go", "a1\na2\na3\n")
	sha := repo.commit(authorAda, addedAt, "add app")
	if len(sha) != 64 {
		t.Fatalf("fixture commit %q is %d chars, want a 64-hex sha256 object name", sha, len(sha))
	}

	got, err := repo.connector().Blame(context.Background(), "app.go", 1, 3)
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	assertSpans(t, got, []BlameSpan{
		{SHA: sha, LineStart: 1, LineEnd: 3, Author: authorAda, Time: mustTime(t, addedAt)},
	})
}

func TestBlameRejectsContentBeforeAnyHeader(t *testing.T) {
	got, err := parseBlamePorcelain("\ta1\n\ta2\n")
	if err == nil {
		t.Fatalf("parseBlamePorcelain of headerless output = %+v, want an error", got)
	}
}
