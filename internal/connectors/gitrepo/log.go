package gitrepo

import (
	"context"
	"fmt"
	"strings"
)

const logFormat = "%H%x00%an%x00%at%x00%s"

func (r *Repo) Log(ctx context.Context, file string) ([]CommitRef, error) {
	rel, err := repoRelPath(file)
	if err != nil {
		return nil, err
	}
	// --follow works with exactly one pathspec, which is all this call ever passes.
	out, err := r.run(ctx, "log", "--follow", "--format="+logFormat, "--", rel)
	if err != nil {
		if r.headUnborn(ctx) {
			return nil, nil
		}
		return nil, err
	}
	return parseLog(string(out))
}

func parseLog(out string) ([]CommitRef, error) {
	refs := make([]CommitRef, 0, strings.Count(out, "\n"))
	for rest := strings.TrimSuffix(out, "\n"); rest != ""; {
		var record string
		record, rest, _ = strings.Cut(rest, "\n")

		fields := strings.SplitN(record, "\x00", 4)
		if len(fields) != 4 {
			return nil, fmt.Errorf("git log record %q carries %d fields, want 4", record, len(fields))
		}
		when, err := epochTime(fields[2])
		if err != nil {
			return nil, err
		}
		refs = append(refs, CommitRef{SHA: fields[0], Author: fields[1], Time: when, Subject: fields[3]})
	}
	return refs, nil
}
