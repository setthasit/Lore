package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/setthasit/Lore/sdk"
)

// Repo is the git implementation of lore.CodeRepo. Every path it takes is
// relative to the root of the single clone it is bound to.
var _ lore.CodeRepo = (*Repo)(nil)

type Repo struct {
	root string
}

func New(root string) *Repo { return &Repo{root: root} }

func (r *Repo) HasFileAtHEAD(ctx context.Context, file string) (bool, error) {
	rel, err := repoRelPath(file)
	if err != nil {
		return false, err
	}
	out, err := r.run(ctx, "ls-tree", "-z", "HEAD", "--", rel)
	if err != nil {
		if r.headUnborn(ctx) {
			return false, nil
		}
		return false, err
	}
	entry, _, _ := strings.Cut(string(out), "\x00")
	fields := strings.Fields(entry)
	return len(fields) > 1 && fields[1] == "blob", nil
}

// An otherwise healthy clone whose HEAD has no commit yet exits 1 here; a missing
// or unreadable repository exits 128.
func (r *Repo) headUnborn(ctx context.Context) bool {
	_, err := r.run(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	var gitErr *gitError
	return errors.As(err, &gitErr) && gitErr.code == 1
}

// An inherited GIT_DIR would silently point every command at another repository;
// GIT_OPTIONAL_LOCKS=0 keeps these read-only commands off the clone's index lock.
var gitEnv = []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"}

func (r *Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.root}, args...)...)
	cmd.Env = gitEnv
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	switch {
	case err == nil:
		return stdout.Bytes(), nil
	case ctx.Err() != nil:
		return nil, fmt.Errorf("git %s: %w", args[0], ctx.Err())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil, &gitError{args: args, code: exit.ExitCode(), stderr: strings.TrimSpace(stderr.String())}
	}
	return nil, fmt.Errorf("git %s: %w", args[0], err)
}

type gitError struct {
	args   []string
	code   int
	stderr string
}

func (e *gitError) Error() string {
	if e.stderr == "" {
		return fmt.Sprintf("git %s: exit status %d", strings.Join(e.args, " "), e.code)
	}
	return fmt.Sprintf("git %s: exit status %d: %s", strings.Join(e.args, " "), e.code, e.stderr)
}

func repoRelPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty file path")
	}
	if strings.ContainsRune(p, '\\') {
		return "", fmt.Errorf("file path %q: git separates path components with /", p)
	}
	// Every check below reads the cleaned value, because that is what git receives.
	clean := path.Clean(p)
	switch {
	case filepath.IsAbs(clean) || strings.HasPrefix(clean, "/"):
		return "", fmt.Errorf("file path %q is absolute, want one relative to the repository root", p)
	case strings.HasPrefix(clean, ":"):
		// "--" ends option parsing but not pathspec magic: ":(exclude)x" would still be honoured.
		return "", fmt.Errorf("file path %q must not begin with ':'", p)
	case clean == "." || clean == ".." || strings.HasPrefix(clean, "../"):
		return "", fmt.Errorf("file path %q does not name a file inside the repository", p)
	}
	return clean, nil
}

func epochTime(value string) (time.Time, error) {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("commit time %q: %w", value, err)
	}
	return time.Unix(seconds, 0).UTC(), nil
}
