// Package github ingests commits, pull requests, reviews, review comments,
// issues and issue comments from GitHub.
//
// It reads GraphQL for objects and one REST endpoint for a commit's touched-file
// list, which GraphQL does not expose. Authentication is a personal access
// token, passed to [NewConnector] as a value: reading it from the environment is
// the injector's job. The token reaches nothing but the Authorization header —
// this package never logs, and no error message carries a header.
//
// Ingest scope is the configured repository list and is independent of local
// clones; no repository has to exist on disk.
//
// # External ids
//
// A DocID is "github:<type>:<external id>", and external ids mirror the web URL
// path so a comment's thread is its id with the fragment stripped:
//
//	commit          owner/repo/commit/<sha>
//	pr              owner/repo/pull/<number>
//	issue           owner/repo/issues/<number>
//	pr_review       owner/repo/pull/<number>#pullrequestreview-<id>
//	review_comment  owner/repo/pull/<number>#discussion_r<id>
//	issue_comment   owner/repo/issues/<number>#issuecomment-<id>
//
// Non-comment types carry no fragment and are their own thread. The fragment
// comes from the object's own web URL, falling back to its database id.
//
// # Timestamps
//
// Every document sets both timestamps. Pull requests, issues, reviews and
// comments use the API's createdAt and updatedAt. A commit uses its author date
// as CreatedAt and its committed date as UpdatedAt — the committed date is what
// GitHub filters history on, so it is the watermark. When a source field is
// absent, the other timestamp fills it.
//
// # Cursor
//
// The cursor holds two keys per repository:
//
//	"owner/repo:updated_at"  RFC 3339 watermark of the last yielded item
//	"owner/repo:doc_id"      that item's DocID
//
// The pair is a total order over a repository's top-level items. The document id
// is load-bearing: GitHub timestamps have second precision, and without the
// tiebreak a watermark could not tell an already-yielded item from a different
// one updated in the same second. Re-running a committed cursor therefore yields
// nothing already seen, while an uncommitted batch is simply re-fetched — and
// upserts are idempotent by DocID either way.
//
// Commits are the one exception: they are exempt from the tiebreak and re-enter
// the stream whenever their committed date equals the watermark's second. A
// commit is immutable and can be pushed long after that date, so one hidden by
// the tiebreak would never come back, whereas a pull request or issue in the
// same position re-enters on its next edit. The price is at most a document or
// two re-yielded per round, which upserts absorb.
//
// Reviews and comments have no watermark of their own: they ride along with
// their parent, whose updatedAt GitHub bumps when they change.
//
// # References
//
// Explicit API relations are emitted first: a pull request's commit SHAs
// (full 40 characters) and its closing-issue numbers, and a commit's associated
// pull request numbers. Text is then scanned for ticket keys (PROJ-123), URLs,
// "#123" and "owner/repo#123" cross-references, and 7-to-40-character hex SHAs;
// pull requests also scan the head branch name. Commits contribute their touched
// paths, review comments the path they annotate. References are deduplicated per
// document. Precision is the LinkResolver's problem — an unresolvable reference
// stays pending and never becomes an edge.
//
// # Limits
//
// GitHub returns these connections newest-first and offers no ascending order
// for commit history, so a watermark sync reads the changed prefix and reverses
// it, buffering one round's documents per repository; the first backfill of a
// large repository is the peak. A commit pushed with a committed date older than
// the watermark's second stays invisible, which is inherent to a committed-date
// watermark; a tie with that second is replayed instead (see Cursor). Only
// default-branch history is ingested.
//
// Three reference sources are read without pagination, so a pathological object
// contributes fewer refs than it could: a commit's touched-file list is REST's
// first page (about 300 files), and a commit's associated pull requests and a
// pull request's closing issues take their first 10 and 20 entries. Documents
// and their bodies are never truncated.
package github
