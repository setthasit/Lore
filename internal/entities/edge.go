package entities

// EdgeKind names a typed relationship between two documents.
type EdgeKind string

const (
	EdgeKindCommitInPR     EdgeKind = "commit_in_pr"
	EdgeKindPRClosesIssue  EdgeKind = "pr_closes_issue"
	EdgeKindReferencesDoc  EdgeKind = "references_doc"
	EdgeKindMentionsCommit EdgeKind = "mentions_commit"
	EdgeKindMentionsPath   EdgeKind = "mentions_path"
	EdgeKindSupersedes     EdgeKind = "supersedes"
)

// Edge is a directional relationship produced by the LinkResolver: Src contains
// the reference, Dst is the referenced document.
type Edge struct {
	Src        DocID
	Dst        DocID
	Kind       EdgeKind
	Confidence float32 // 1.0 explicit API link … 0.5 fuzzy text match
}

// DirOut walks Src→Dst, DirIn walks Dst→Src, DirBoth unions the two.
type Direction int

const (
	DirOut Direction = iota
	DirIn
	DirBoth
)

// A RawRef whose target is not ingested yet; every sync round retries it.
type PendingRef struct {
	SourceDoc DocID
	Ref       RawRef
}
