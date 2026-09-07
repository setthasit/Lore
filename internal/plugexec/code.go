package plugexec

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/setthasit/Lore/sdk"
)

// codeRepo is one registered clone. A clone has no instance id of its own — the
// registry names its logger after the plugin — so that name is what errors are
// attributed to, and the absolute path in the message says which clone.
type codeRepo struct {
	external
	root string
}

var _ lore.CodeRepo = (*codeRepo)(nil)

func (r *codeRepo) Blame(ctx context.Context, path string, startLine, endLine int) ([]lore.BlameSpan, error) {
	absolute, err := r.resolve(opBlame, path)
	if err != nil {
		return nil, err
	}

	frame, err := r.unary(ctx, r.manifest.Name, opBlame, r.tuning.unary, func(env envelope) any {
		return blameRequest{envelope: env, Path: absolute, StartLine: startLine, EndLine: endLine}
	})
	if err != nil {
		return nil, err
	}
	return frame.Spans, nil
}

func (r *codeRepo) Log(ctx context.Context, path string) ([]lore.CommitRef, error) {
	absolute, err := r.resolve(opLog, path)
	if err != nil {
		return nil, err
	}

	frame, err := r.unary(ctx, r.manifest.Name, opLog, r.tuning.unary, func(env envelope) any {
		return pathRequest{envelope: env, Path: absolute}
	})
	if err != nil {
		return nil, err
	}
	// A path with no history is not an error, so an empty commit list is an
	// answer and the caller is left to decide what it means.
	return frame.Commits, nil
}

// HasFileAtHEAD is its own op rather than an inference from an empty Log,
// because a deleted file still has history: the query engine asks it before
// blaming so a mistyped path comes back as a missing file instead of a raw tool
// failure. A directory, an untracked path and a clone with no commits are all
// false and none of them is an error.
func (r *codeRepo) HasFileAtHEAD(ctx context.Context, path string) (bool, error) {
	absolute, err := r.resolve(opHasFile, path)
	if err != nil {
		return false, err
	}

	frame, err := r.unary(ctx, r.manifest.Name, opHasFile, r.tuning.unary, func(env envelope) any {
		return pathRequest{envelope: env, Path: absolute}
	})
	if err != nil {
		return false, err
	}
	return frame.Present, nil
}

// Paths crossing the protocol are slash-separated whatever the host is.
func (r *codeRepo) resolve(op, path string) (string, error) {
	if path == "" {
		return "", protocolError(r.manifest.Name, op, nil, "no path to read in the clone at %s", r.root)
	}
	if strings.ContainsRune(path, '\\') {
		return "", protocolError(r.manifest.Name, op, nil, "path %q separates components with \\, want /", path)
	}
	if strings.HasPrefix(path, "/") || filepath.IsAbs(path) {
		return "", protocolError(r.manifest.Name, op, nil, "path %q is absolute, want one relative to the clone at %s", path, r.root)
	}

	absolute := filepath.Join(r.root, filepath.FromSlash(path))
	relative, err := filepath.Rel(r.root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", protocolError(r.manifest.Name, op, nil, "path %q climbs out of the clone at %s", path, r.root)
	}
	return absolute, nil
}
