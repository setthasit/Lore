# Demo: why is this line here?

The code-anchored walkthrough. You point Lore at a span of code in a local clone, and it
answers with the decision trail behind those lines — the commits that wrote them, the pull
request that carried the change, and the issue that asked for it — each entry with a URL
you can open.

This walkthrough uses a real, public anchor so nothing here has to be taken on faith. It
is a **read-only** demo: Lore only reads GitHub, and the clone is only read by `git blame`
and `git log`.

## The anchor

[`cli/cli`](https://github.com/cli/cli) is the GitHub CLI: Go, merge-commit workflow, every
change lands through a pull request, and most pull requests close an issue. That discipline
is what makes a provenance chain exist at all.

The line we anchor on is an admitted workaround, three comment lines deep inside a GraphQL
query struct:

| | |
|---|---|
| Repository | [`cli/cli`](https://github.com/cli/cli) |
| Pinned at | `adda317c11b892da5e5ed3fbbf26b69a5c163cc6` |
| File | `api/queries_pr_review.go` |
| Span | lines `573-577` |
| Blamed commits | [`8f62e8116df4`](https://github.com/cli/cli/commit/8f62e8116df4243019a41681ec54c66dda2e8f2e) (2026-03-05), [`72a6e9f3a7c5`](https://github.com/cli/cli/commit/72a6e9f3a7c533139a475c482b0e179574b2ff58) (2026-02-13) |
| Pull request | [cli/cli#12627](https://github.com/cli/cli/pull/12627) — *`gh pr create`: login-based reviewer requests and search-based interactive selection* |
| Issue | [cli/cli#11501](https://github.com/cli/cli/issues/11501) — *`gh pr create --reviewer ...` is slow in repos with many possible reviewers* |

The lines themselves:

```
573: 			// HACK: There's no repo-level API to check Copilot reviewer eligibility,
574: 			// so we piggyback on an open PR's suggestedReviewerActors to detect
575: 			// whether Copilot is available as a reviewer for this repository.
576: 			PullRequests struct {
577: 				Nodes []struct {
```

**The one-sentence why:** the query fetches `pullRequests(first: 1, states: [OPEN])` it does
not otherwise need, because making `gh pr create --reviewer` fast (#11501) meant dropping
the paginated fetch of every assignable user — and there is no repo-level API that reports
Copilot reviewer eligibility without a pull request to hang it on.

Nothing in the code says that. The comment says *what* the hack is; only the pull request
body and the issue say *why* anyone accepted it. That is the gap `lore why` closes.

## Prerequisites

| | |
|---|---|
| `lore` | `go install github.com/setthasit/Lore/cmd/lore@latest` |
| `git` | on `PATH` — the blame and log connector shells out to it |
| A GitHub token | Read-only, exported under the name you put in `token_env`. `cli/cli` is public, so a fine-grained token scoped to *public repositories, read-only* is enough — it is there to lift the API rate limit, and the connector never writes |
| An embedder | `lore why` embeds its retrieval query, so the embedder must be reachable. `embedder: openai` needs `OPENAI_API_KEY`; a local Ollama needs the daemon up — see [`fully-local.md`](fully-local.md) |
| An LLM | **optional**, and only for `--explain` |

## Clone and pin

The clone lives wherever you like; it is never written to. Blame is taken at the clone's
`HEAD`, so pin `HEAD` to the commit this walkthrough was verified against — otherwise later
commits shift the line numbers and `573-577` stops being the hack:

```
git clone --filter=blob:none https://github.com/cli/cli.git ~/dev/cli
git -C ~/dev/cli checkout adda317c11b892da5e5ed3fbbf26b69a5c163cc6
git -C ~/dev/cli blame -L 573,577 -- api/queries_pr_review.go
```

That last line is the sanity check: it must attribute 573–575 to `8f62e811…` and 576–577 to
`72a6e9f3…` (git picks its own abbreviation width). A partial clone (`--filter=blob:none`)
is fine — git fetches the blobs blame needs on demand.

## `lore.yaml`

Two blocks matter, and they are independent. `sources.github` says what to **ingest** — it
needs no clone. `repos[]` registers the **clone** for blame and file history, and its
`remote` is what ties the two together: the blamed SHA is looked up as a `github:cli/cli`
document.

```yaml
workspace: cli-demo

sources:
  github:
    token_env: LORE_GITHUB_TOKEN
    repos:
      - cli/cli

repos:
  - path: ~/dev/cli
    remote: github:cli/cli

embedder:
  provider: openai
  model: text-embedding-3-small

# Only needed for --explain.
# llm:
#   provider: anthropic
#   model: claude-sonnet-4-5
#   api_key_env: LORE_LLM_KEY
```

`lore init` scaffolds this file with the `llm:`, `query:` and `scheduler:` blocks commented
out; the values above are the edits. Every command takes `--config`, defaulting to
`./lore.yaml`.

Two things the loader enforces before anything runs: `sources.github.token_env` must name a
variable that is actually **set** in the environment (`sources.github.token_env names
LORE_GITHUB_TOKEN, but that environment variable is not set`), and every `repos[].path`
must exist and contain a `.git` entry (`repos path /home/dev/cli is not a git repository —
no .git entry found`). A leading `~` in `repos[].path` and `index_path` is expanded before
validation, which is why that message names the absolute path. Unknown keys are rejected
outright.

```
export LORE_GITHUB_TOKEN=...   # read-only
export OPENAI_API_KEY=...
lore status
```

## Sync, honestly

```
lore sync
```

`cli/cli` is a large repository, and the first sync is a **full backfill** — there is no
date or count knob to narrow it. As of the pinned commit, `trunk` carries roughly 12,000
commits, and the repository has ~4,500 pull requests and ~6,400 issues (GitHub counts read
on 2026-09-02; they only grow); every pull
request also pulls its review threads and every issue its comments. The connector pages
50 commits, 20 pull requests and 50 issues per GraphQL request, with nested pages of 20 for
reviews, review comments and issue comments — so the backfill is many hundreds of API
round trips, and every document it produces is chunked and embedded. Budget **hours**, not
minutes, and expect real embedding spend on a paid provider.

What actually helps:

| Lever | Effect |
|---|---|
| List exactly one repository under `sources.github.repos` | The connector walks repositories in order; each extra one is another full history |
| Local embedder (`embedder.provider: ollama`) | The backfill still takes hours of API paging, but costs nothing per chunk. See [`fully-local.md`](fully-local.md) |
| Just let it run once | Sync checkpoints per batch, so `Ctrl-C` and re-running resumes at the watermark; nothing already indexed is refetched. Later syncs are incremental — commits and issues are filtered server-side by the watermark, and the pull-request walk stops at the first PR older than it |
| Run the walkthrough on a small repository of your own | The commands are identical; only the anchor changes |

One consequence worth knowing before you interrupt a backfill: within a repository, items
are ingested **oldest first**. The two commits we anchor on are from 2026, so they are
among the *last* things indexed. Stop the backfill early and `lore why` still blames
correctly — it just reports the trail as missing (see [When the trail is
thin](#when-the-trail-is-thin)).

When it finishes, `lore sync` prints:

```
sync complete — `lore status` for counts and cursor ages
```

## `lore why`

```
lore why api/queries_pr_review.go:573-577
```

The path is relative to the repository root, never absolute. The span is the part after the
**last** colon, so paths containing colons still parse. `--repo` names the clone — by
remote (`github:cli/cli`) or by path — and can be omitted here because the workspace
registers exactly one.

The output is a timeline. Shape, annotated — not a recorded transcript; the exact set and
order of entries depends on your index:

```
why does api/queries_pr_review.go:573-577 exist in its current form
anchor: github:cli/cli api/queries_pr_review.go:573-577
        blamed 8f62e8116df4, 72a6e9f3a7c5

N documents

2026-03-05 Label Copilot detection in SuggestedReviewerActorsForRepo as a hack
   github commit · BagToad · 2026-03-05 · blamed_commit
   https://github.com/cli/cli/commit/8f62e8116df4243019a41681ec54c66dda2e8f2e
      			// HACK: There's no repo-level API to check Copilot reviewer eligibility,
      			// so we piggyback on an open PR's suggestedReviewerActors to detect
      			// whether Copilot is available as a reviewer for this repository.

2026-02-13 Move PR review queries from queries_pr.go to queries_pr_review.go
   github commit · BagToad · 2026-02-13 · blamed_commit
   https://github.com/cli/cli/commit/72a6e9f3a7c533139a475c482b0e179574b2ff58
      			PullRequests struct {
      				Nodes []struct {

2026-02-06 `gh pr create`: login-based reviewer requests and search-based interactive selection
   github pr · BagToad · 2026-02-06 · linked_change
   https://github.com/cli/cli/pull/12627

2025-08-13 `gh pr create --reviewer ...` is slow in repos with many possible reviewers
   github issue · jakub-g · 2025-08-13 · linked_ticket
   https://github.com/cli/cli/issues/11501

chains:
  github:commit:cli/cli/commit/8f62e8116df4243019a41681ec54c66dda2e8f2e → github:pr:cli/cli/pull/12627 → github:issue:cli/cli/issues/11501
```

How to read it:

| Element | Meaning |
|---|---|
| First line | The question. Without a question argument it is generated: `why does <file>:<L1>-<L2> exist in its current form` |
| `anchor:` | The registered repository name and the span. The name is `remote` when set, otherwise the clone path |
| `blamed` | One SHA per blamed commit, abbreviated to 12 characters, in **first-blamed order** — the order the lines appear in the span, not chronological |
| `N documents` | Entry count (`1 document` when there is one) |
| Entry lead | The document's creation date, `YYYY-MM-DD`, or `undated` |
| Meta line | `source type · author · date · role` — the author and date parts drop out when the document has none, and the role part when it is the plain `seed` |
| Indented block | The excerpt. For a blamed commit it is the source lines *that commit owns*, verbatim including their original tabs, indented six spaces — which is why the two commits show different lines of the same span |
| `chains:` | Document ids walked end to end. This is the provenance claim: commit → PR → issue |

Roles are how far from the anchor a document sits: `blamed_commit` is the anchor itself,
`linked_change` a commit or pull request reached by a link, `linked_ticket` an issue or
ticket, `review_thread` a review or comment, `semantic_match` a document that matched the
query text rather than a link. Entries are ranked by score, then by recency — blamed
commits always lead, because they are the anchor. Pull request #12627 carried **nine**
review threads when this walkthrough was verified, so a fully synced index will also list those as `review_thread`, and
`query.top_k` (default 12) semantic matches can add documents no link reached.

Two shorthands:

```
lore why api/queries_pr_review.go:573                      # one line; the anchor still reads 573-573
lore why api/queries_pr_review.go:573-577 "why fetch an open PR here?"
```

The second form replaces the generated question, which changes both the retrieval text and
what `--explain` is asked to answer.

## `lore history`

`why` explains a span. `history` walks the whole file, newest commit first, and needs **no
embedder** — it is `git log --follow` plus a graph walk:

```
lore history api/queries_pr_review.go
```

```
history of api/queries_pr_review.go in github:cli/cli
anchor: github:cli/cli api/queries_pr_review.go
        blamed <one 12-character SHA per commit in the page, newest first>
<the timeline, one entry per indexed commit and per linked document>
```

The anchor carries no line span here, so it prints the bare path. Pagination is explicit:

| Flag | Behavior |
|---|---|
| `--limit N` | Commits per page. Default 20, capped at 50 — ask for more and you get 50 |
| `--before SHA` | The page holds the commits **older** than `SHA`. Full or abbreviated |

To walk back, pass the **last SHA of the `blamed` line** to `--before`:

```
lore history api/queries_pr_review.go --limit 10
lore history api/queries_pr_review.go --limit 10 --before 72a6e9f3a7c5
```

An empty `blamed` line means the history is exhausted. An abbreviation that matches two
commits in the file's log is refused rather than guessed.

## The follow-ups

`why` hands you SHAs and URLs; the other two verbs take them.

```
lore trace 8f62e8116df4243019a41681ec54c66dda2e8f2e
```

`trace` is depth on one document: it resolves the ref and prints everything linked to it —
in both directions by default, `--direction out` for what it references, `in` for what
references it. It resolves a full or abbreviated commit SHA, a PR or issue number, a
ticket key, a document URL, or a document id, and it never touches the embedder.

```
lore impact https://github.com/cli/cli/pull/12627
lore impact https://github.com/cli/cli/pull/12627 --question "did the Copilot detection change again?"
```

`impact` is the forward view: what came *after* that decision. Prefer the URL or the
document id (`github:pr:cli/cli/pull/12627`) over a bare number — a number is looked up as
both a pull request and an issue, and if both exist you get:

> ref "12627" matches 2 documents — retry with one of: ...

## `--explain` and `--raw`

All four evidence verbs — `why`, `trace`, `impact`, `history` — take the same two output
flags.

```
lore why api/queries_pr_review.go:573-577 --explain
```

`--explain` answers from the trail in prose instead of printing the timeline. It needs the
`llm:` block; without one it fails with:

> synthesis needs an LLM, and this workspace has no llm: block in lore.yaml — add one naming the provider, the model and the api_key_env that holds its key

```
lore why api/queries_pr_review.go:573-577 --raw | jq '.chains, .anchor.blamed_shas'
```

`--raw` emits the evidence bundle as JSON: `question`, `anchor` (with
`code.{repo,file,line_start,line_end,blamed_shas}`), `nodes[]` (each
`doc.{id,source,type,title,author,url,created_at,updated_at}`, `excerpt`, `role`, `score`,
`via`), and the optional `chains` and `gaps`. This is the same bundle the MCP `why` tool
returns. `--raw` wins when both flags are given, so it is safe to script.

## When the trail is thin

Lore reports missing provenance instead of inventing it.

**A commit the index never ingested.** Blame still works — the commit is just not a
document, so nothing links from it:

```
gaps:
  trail ends at commit 8f62e8116df4, not synced from a source
```

That is the expected output of an interrupted backfill, of a span whose commits live on a
branch that never reached `trunk`, and of a repository you registered as a clone but never
listed under `sources.github.repos`.

**A repository with no PR discipline.** Direct pushes, no pull request, no issue: the
commit is indexed but references nothing. The commit appears as `blamed_commit`, there is
no `chains:` block, and each unlinked commit gets a gap naming itself:

```
gaps:
  Bump deps (github:commit:acme/tool/commit/1a2b3c4d5e6f708192a3b4c5d6e7f80910111213) stands alone; no linked discussion
```

**No evidence at all.** When the bundle cites nothing:

```
no evidence found — nothing links to this document yet, or run `lore sync` if the trail should be there
```

**No clone registered.** `why` and `history` cannot anchor on code in an ask-only
workspace. This is a precondition failure, exit code 3, printed on stderr with nothing on
stdout:

```
lore: no repositories registered — code anchoring disabled for this workspace
```

Ask instead — `lore ask` needs no clone.

**A `remote` that names no ingested repository.** This is a warning, not an error: blame
works, but the blamed SHA resolves to no document, so the chain cannot start. Every command
in that workspace prints it on stderr before doing its work:

```
lore: warning: repos path /home/dev/cli has remote github:cli/cli, which names no configured source repo — blame still works, but chains stop at the commit layer
```

Usual causes: a typo, `remote:` missing its `github:` prefix, or a `repos[]` entry whose
repository is not in `sources.github.repos`. Owner and name are matched case-insensitively.

**A malformed span** is rejected before the workspace is even opened (exit code 2):

```
lore: argument "api/queries_pr_review.go" names no line span: write it as <file>:<line> or <file>:<first>-<last>
```

Same shape for `does not number its line span`, `ends its line span at line zero` and
`ends its line span before it starts`.

**A path not tracked at `HEAD`** — renamed after the pin, or misspelled — is a not-found
(exit code 4):

```
lore: api/queries_pr_reviews.go is not tracked at HEAD of github:cli/cli
```

## Recording the demo

Producing the asciinema cast or GIF is a **human step** — Lore ships no recorder. Sync
first, off-camera: a multi-hour backfill is not the demo.

```
# Off camera: prepare, and verify the anchor still blames as expected.
export LORE_GITHUB_TOKEN=...  OPENAI_API_KEY=...
git -C ~/dev/cli rev-parse HEAD          # adda317c11b892da5e5ed3fbbf26b69a5c163cc6
git -C ~/dev/cli blame -L 573,577 -- api/queries_pr_review.go
lore sync && lore status

# On camera.
asciinema rec lore-why.cast --idle-time-limit 1.5
```

Inside the recording, in order:

```
sed -n '570,578p' ~/dev/cli/api/queries_pr_review.go
lore why api/queries_pr_review.go:573-577
lore why api/queries_pr_review.go:573-577 --explain
lore trace 8f62e8116df4243019a41681ec54c66dda2e8f2e
lore impact https://github.com/cli/cli/pull/12627
lore history api/queries_pr_review.go --limit 5
exit
```

Then convert and check that the URLs are legible at the size you publish:

```
agg lore-why.cast lore-why.gif --font-size 16
```

The narrative beat is the first two commands: the comment admits a hack and names no
reason; `lore why` produces the pull request that traded the roundtrip away and the issue
that asked for the speed.

## See also

- [`demo-ask-only.md`](demo-ask-only.md) — the same story without a clone, anchored on documents
- [`sources.md`](sources.md) — what each connector ingests and the token each one needs
- [`quickstart-mcp.md`](quickstart-mcp.md) — the `why` tool over MCP, returning the same bundle
- [`fully-local.md`](fully-local.md) — running the embedder and LLM on your own machine
