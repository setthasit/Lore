package plugexec

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/wire"
)

type codeRepo struct {
	external
	root string
}

var _ lore.CodeRepo = (*codeRepo)(nil)

func (r *codeRepo) Blame(ctx context.Context, path string, startLine, endLine int) ([]lore.BlameSpan, error) {
	absolute, err := r.resolve(wire.OpBlame, path)
	if err != nil {
		return nil, err
	}

	frame, err := r.unary(ctx, r.manifest.Name, wire.OpBlame, r.tuning.unary, func(env wire.Envelope) any {
		return wire.BlameRequest{Envelope: env, Path: absolute, StartLine: startLine, EndLine: endLine}
	})
	if err != nil {
		return nil, err
	}
	return frame.Spans, nil
}

func (r *codeRepo) Log(ctx context.Context, path string) ([]lore.CommitRef, error) {
	absolute, err := r.resolve(wire.OpLog, path)
	if err != nil {
		return nil, err
	}

	frame, err := r.unary(ctx, r.manifest.Name, wire.OpLog, r.tuning.unary, func(env wire.Envelope) any {
		return wire.PathRequest{Envelope: env, Path: absolute}
	})
	if err != nil {
		return nil, err
	}
	return frame.Commits, nil
}

func (r *codeRepo) HasFileAtHEAD(ctx context.Context, path string) (bool, error) {
	absolute, err := r.resolve(wire.OpHasFile, path)
	if err != nil {
		return false, err
	}

	frame, err := r.unary(ctx, r.manifest.Name, wire.OpHasFile, r.tuning.unary, func(env wire.Envelope) any {
		return wire.PathRequest{Envelope: env, Path: absolute}
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
