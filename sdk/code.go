package lore

import (
	"context"
	"time"
)

// CodeRepo answers blame and history questions about one local clone. Every
// path is relative to the root of the clone the host bound this value to, and
// every operation is read-only: a code plugin never writes to the clone.
type CodeRepo interface {
	// An endLine past the last line of the file is clamped to it; a startLine past it is an error.
	Blame(ctx context.Context, path string, startLine, endLine int) ([]BlameSpan, error)

	// Newest first, following the file across renames. A path with no history is not an error.
	Log(ctx context.Context, path string) ([]CommitRef, error)

	// False rather than an error for a directory, an untracked file, or a clone with no commits.
	HasFileAtHEAD(ctx context.Context, path string) (bool, error)
}

// LineStart and LineEnd are 1-based and inclusive; Time is the author time in
// UTC. Lines is the blamed source text, one entry per line in span order.
type BlameSpan struct {
	SHA       string
	LineStart int
	LineEnd   int
	Author    string
	Time      time.Time
	Lines     []string
}

// Time is the author time in UTC.
type CommitRef struct {
	SHA     string
	Author  string
	Time    time.Time
	Subject string
}
