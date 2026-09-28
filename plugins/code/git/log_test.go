package git

import (
	"context"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

// awkwardSubject packs every delimiter a naive split would trip over.
const awkwardSubject = "fix: 100% |pipe|\ttab %H \"quoted\" 'single' \\n a,b;c"

func TestLogFollowsRenamesNewestFirst(t *testing.T) {
	repo := newTestRepo(t)
	repo.write("old.go", "package old\n\nfunc Old() {}\n")
	shaAdd := repo.commit(authorAda, "2024-04-01T00:00:00Z", "add old")
	repo.write("old.go", "package old\n\nfunc Old() int { return 1 }\n")
	shaEdit := repo.commit(authorGrace, "2024-04-02T08:15:00+02:00", awkwardSubject)
	repo.git("mv", "old.go", "new.go")
	shaRename := repo.commit(authorAda, "2024-04-03T00:00:00Z", "rename old to new")

	got, err := repo.connector().Log(context.Background(), "new.go")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	want := []lore.CommitRef{
		{SHA: shaRename, Author: authorAda, Time: mustTime(t, "2024-04-03T00:00:00Z"), Subject: "rename old to new"},
		{SHA: shaEdit, Author: authorGrace, Time: mustTime(t, "2024-04-02T08:15:00+02:00"), Subject: awkwardSubject},
		{SHA: shaAdd, Author: authorAda, Time: mustTime(t, "2024-04-01T00:00:00Z"), Subject: "add old"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d commits, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.SHA != w.SHA || g.Author != w.Author || g.Subject != w.Subject || !g.Time.Equal(w.Time) {
			t.Errorf("commit %d\n got %+v\nwant %+v", i, g, w)
		}
	}
}

func TestLogWithoutHistory(t *testing.T) {
	repo := newTestRepo(t)
	repo.write("main.go", "package main\n")
	repo.commit(authorAda, "2024-04-01T00:00:00Z", "init")

	got, err := repo.connector().Log(context.Background(), "never-existed.go")
	if err != nil {
		t.Fatalf("Log of a path with no history: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d commits, want none: %+v", len(got), got)
	}
}

func TestLogWithoutCommits(t *testing.T) {
	repo := newTestRepo(t)
	repo.write("main.go", "package main\n")

	got, err := repo.connector().Log(context.Background(), "main.go")
	if err != nil {
		t.Fatalf("Log on a repo with no commits: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d commits, want none: %+v", len(got), got)
	}
}

func TestLogOutsideRepository(t *testing.T) {
	got, err := New(t.TempDir()).Log(context.Background(), "main.go")
	if err == nil {
		t.Fatalf("Log outside a repository = %+v, want an error", got)
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error %q should carry git's stderr", err)
	}
}
