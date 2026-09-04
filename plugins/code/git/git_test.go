package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	authorAda    = "Ada Lovelace"
	authorGrace  = "Grace Hopper"
	fixtureEmail = "fixture@example.invalid"
)

// testRepo scripts a throwaway clone with a pinned identity, timestamp and branch
// so assertions on author and time hold on a machine with no git configuration.
type testRepo struct {
	t    *testing.T
	root string
	home string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	r := &testRepo{t: t, root: t.TempDir(), home: t.TempDir()}
	r.git("init", "--initial-branch=main", "--quiet")
	return r
}

func newSHA256TestRepo(t *testing.T) *testRepo {
	t.Helper()
	r := &testRepo{t: t, root: t.TempDir(), home: t.TempDir()}
	cmd := exec.Command("git", "-C", r.root, "init", "--initial-branch=main", "--quiet", "--object-format=sha256")
	cmd.Env = hermeticEnv(r.home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git cannot create a sha256 repository: %v\n%s", err, out)
	}
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	return r.gitWithEnv(nil, args...)
}

func (r *testRepo) gitWithEnv(extra []string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.root}, args...)...)
	cmd.Env = hermeticEnv(r.home, extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func hermeticEnv(home string, extra ...string) []string {
	return append([]string{
		"LC_ALL=C",
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
	}, extra...)
}

func (r *testRepo) write(rel, content string) {
	r.t.Helper()
	full := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", rel, err)
	}
}

func (r *testRepo) remove(rel string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.root, filepath.FromSlash(rel))); err != nil {
		r.t.Fatalf("remove %s: %v", rel, err)
	}
}

func (r *testRepo) commit(author, when, subject string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.gitWithEnv(
		[]string{"GIT_AUTHOR_DATE=" + when, "GIT_COMMITTER_DATE=" + when},
		"-c", "user.name="+author, "-c", "user.email="+fixtureEmail,
		"commit", "--quiet", "-m", subject,
	)
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

func (r *testRepo) connector() *Repo { return New(r.root) }

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UTC()
}

func TestRepoRelPathRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"/etc/passwd",
		"../../etc/passwd",
		"..",
		"../x",
		"a/../../x",
		"a/b/../../..",
		".",
		"./",
		`a\..\..\etc\passwd`,
		":(exclude)a.go",
		":/",
		"./:(exclude)a.go",
		"a/../:(exclude)x",
		".//:(exclude)x",
		"./../etc/passwd",
	} {
		if got, err := repoRelPath(in); err == nil {
			t.Errorf("repoRelPath(%q) = %q, want an error", in, got)
		}
	}
}

func TestRepoRelPathAccepts(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"main.go", "main.go"},
		{"a/..b/c", "a/..b/c"},
		{"a/b..c/d", "a/b..c/d"},
		{"..hidden", "..hidden"},
		{"./a/b", "a/b"},
		{"a/./b", "a/b"},
		{"a/b/", "a/b"},
		{"a/b/../c", "a/c"},
	} {
		got, err := repoRelPath(tt.in)
		if err != nil {
			t.Errorf("repoRelPath(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("repoRelPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUnsafePathRejectedByEveryMethod(t *testing.T) {
	repo := newTestRepo(t)
	repo.write("main.go", "package main\n")
	repo.commit(authorAda, "2024-01-01T00:00:00Z", "init")
	git := repo.connector()
	ctx := context.Background()

	const escape = "../../etc/passwd"
	if spans, err := git.Blame(ctx, escape, 1, 1); err == nil {
		t.Errorf("Blame(%q) = %v, want an error", escape, spans)
	}
	if refs, err := git.Log(ctx, escape); err == nil {
		t.Errorf("Log(%q) = %v, want an error", escape, refs)
	}
	if ok, err := git.HasFileAtHEAD(ctx, escape); err == nil {
		t.Errorf("HasFileAtHEAD(%q) = %v, want an error", escape, ok)
	}
}

func TestHasFileAtHEAD(t *testing.T) {
	repo := newTestRepo(t)
	repo.write("main.go", "package main\n")
	repo.write("pkg/lib.go", "package pkg\n")
	repo.write("gone.go", "package gone\n")
	repo.commit(authorAda, "2024-01-01T00:00:00Z", "init")
	repo.write("untracked.go", "package untracked\n")
	repo.remove("gone.go")

	git := repo.connector()
	for _, tt := range []struct {
		name string
		path string
		want bool
	}{
		{"tracked file", "main.go", true},
		{"nested file", "pkg/lib.go", true},
		{"directory", "pkg", false},
		{"absent path", "nope.go", false},
		{"untracked file on disk", "untracked.go", false},
		{"deleted in the working tree but present at HEAD", "gone.go", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := git.HasFileAtHEAD(context.Background(), tt.path)
			if err != nil {
				t.Fatalf("HasFileAtHEAD(%q): %v", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("HasFileAtHEAD(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestHasFileAtHEADWithoutCommits(t *testing.T) {
	repo := newTestRepo(t)
	repo.write("main.go", "package main\n")

	got, err := repo.connector().HasFileAtHEAD(context.Background(), "main.go")
	if err != nil {
		t.Fatalf("HasFileAtHEAD on a repo with no commits: %v", err)
	}
	if got {
		t.Error("HasFileAtHEAD = true, want false: an unborn HEAD holds no files")
	}
}

func TestHasFileAtHEADOutsideRepository(t *testing.T) {
	got, err := New(t.TempDir()).HasFileAtHEAD(context.Background(), "main.go")
	if err == nil {
		t.Fatalf("HasFileAtHEAD outside a repository = %v, want an error", got)
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error %q should carry git's stderr", err)
	}
}
