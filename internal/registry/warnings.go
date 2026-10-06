package registry

import (
	"github.com/setthasit/Lore/sdk"
)

type Warnings []string

type CloneCover struct {
	Name   string
	Synced bool
}

func CloneCoverage(clones []LocalClone, sources []lore.Connector) []CloneCover {
	coverage := make([]CloneCover, 0, len(clones))
	for _, clone := range clones {
		name := clone.Remote
		if name == "" {
			name = clone.Field + " (no remote)"
		}
		coverage = append(coverage, CloneCover{
			Name:   name,
			Synced: clone.Remote != "" && ingested(sources, clone.Remote),
		})
	}
	return coverage
}

func UnmatchedRemotes(clones []LocalClone, sources []lore.Connector) Warnings {
	var warnings Warnings
	for _, clone := range clones {
		if clone.Remote == "" || ingested(sources, clone.Remote) {
			continue
		}
		warnings = append(warnings, "repos path "+clone.Path+" has remote "+clone.Remote+
			", which names no configured source repo — blame still works, but chains stop at the commit layer")
	}
	return warnings
}

func ingested(sources []lore.Connector, remote string) bool {
	for _, source := range sources {
		if matcher, ok := source.(lore.RemoteMatcher); ok && matcher.MatchesRemote(remote) {
			return true
		}
	}
	return false
}
