# Demo: ask-only workspace

A reproducible walkthrough on a workspace with **no repository, no clone and no
commits** — two API sources, Jira and Notion, and nothing else. It answers the two
questions the tool exists for:

1. *Why did we choose option B over option A, around the incident?* — a chain that
   starts in Jira, runs through a Notion page and lands back on the Jira incident.
2. *What did that decision cause?* — the chronological consequences of the Notion
   page, ending on a Jira ticket nobody linked back.

The point of the zero-repository setup: the answers cannot come from `git log`.
Every hop is a reference one document made to another, resolved at sync time.

The story is the same one the automated end-to-end suite exercises
(`internal/e2e/ask_only_test.go` against `internal/e2e/testdata/askonly/`), so the
shapes below are the shapes the suite holds the engine to. Which parts are
*asserted* is called out per block.

---

## 1. Prerequisites

| Need | Why | Notes |
| --- | --- | --- |
| `lore` on `PATH` | — | `go install github.com/setthasit/Lore/cmd/lore@latest` |
| A **sandbox** Jira project you own | the three tickets | Jira Cloud; three project keys, `INC`, `ARCH`, `OPS`, or your own — keys must start with an uppercase letter and hold only uppercase letters, digits and `_` |
| A Jira API token + the account e-mail | Basic auth | read-only account is enough; lore never writes |
| A **sandbox** Notion page tree you own | the decision page | plus an internal integration, shared with the root page |
| An embedder | retrieval | `openai` by default; for no-cloud, see [`fully-local.md`](fully-local.md) |
| An LLM *(optional)* | prose answers | only `lore ask` without `--raw`, and `--explain` on `why`/`trace`/`impact`/`history`, need one |

Two properties of the tool that shape this walkthrough:

- **Read-only toward every source.** Nothing in this repository writes to Jira or
  Notion. Seeding is a manual step you perform in your own sandbox, by hand.
- **No secrets in configuration.** `lore.yaml` names environment *variables*; the
  values live in your shell only.

For a run with no third-party API at all — local embedder, local model — configure
the workspace as in [`fully-local.md`](fully-local.md) and follow this walkthrough
unchanged from step 3 on.

## 2. Seed your own sandbox

Copy the six artifacts in [`docs/demo/ask-only/`](demo/ask-only/) into your sandbox
by hand, in the order that directory's [README](demo/ask-only/README.md) gives:

| Order | Artifact | Where |
| --- | --- | --- |
| 1 | [`INC-201`](demo/ask-only/INC-201.md) — the incident | Jira |
| 2 | [`ARCH-88`](demo/ask-only/ARCH-88.md) — option A vs option B, A rejected | Jira |
| 3 | [`Engineering Decisions`](demo/ask-only/notion-engineering-decisions.md) → [`Checkout Reliability`](demo/ask-only/notion-checkout-reliability.md) → [`Adopt option B for checkout writes`](demo/ask-only/notion-adopt-option-b.md) | Notion |
| 4 | [`OPS-410`](demo/ask-only/OPS-410.md) — extend the writer to refunds | Jira |

Order matters for one reason only: `lore impact` walks *forward in time* from its
anchor and reports what is dated strictly after it. Jira and Notion both stamp
`created` themselves and neither lets you backdate, so the decision page has to
exist before `OPS-410` does.

The references that make the trail resolvable:

```
ARCH-88 ──"INC-201"──────────────────────────────────► INC-201
OPS-410 ──URL of the page──► Adopt option B ──"INC-201"──► INC-201
```

`ARCH-88` and the Notion page reference `INC-201` by plain **ticket key**, which
resolves by key. `OPS-410` references the page by **URL**, which resolves by exact
string equality against the URL Notion's API reported — read
[`OPS-410.md`](demo/ask-only/OPS-410.md) before you paste that link.

## 3. Configure the workspace

```bash
mkdir -p ~/demo/lore-askonly && cd ~/demo/lore-askonly
lore init
```

`lore init` writes a commented `lore.yaml` and prints:

```text
wrote ./lore.yaml
next: set the token variables it names, export OPENAI_API_KEY, then run `lore sync`
```

Its scaffold assumes GitHub, and it leaves `llm:`, `query:` and `scheduler:`
commented out. For this demo, replace the whole file:

```yaml
workspace: lore-askonly

# The index is derived data: safe to delete, rebuilt by the next lore sync.
# index_path: ~/.lore/lore-askonly.db

sources:
  jira:
    base_url: https://acme-sandbox.atlassian.net
    email_env: LORE_JIRA_EMAIL
    token_env: LORE_JIRA_TOKEN
    projects:
      - INC
      - ARCH
      - OPS
  notion:
    token_env: LORE_NOTION_TOKEN
    root_pages:
      - 1f2e3d4c5b6a47788990aabbccddeeff   # the "Engineering Decisions" page id

# No local clones. Zero repos is a valid ask-only workspace — and the whole point
# of this one.
repos: []

embedder:
  provider: openai
  model: text-embedding-3-small

# Optional. Without it, `lore ask` needs --raw and the other verbs lose --explain.
llm:
  provider: anthropic
  model: claude-sonnet-4-5
  api_key_env: LORE_LLM_KEY
```

Then export the four variables it names:

```bash
export LORE_JIRA_EMAIL='lore-bot@example.invalid'      # the Jira account's e-mail
export LORE_JIRA_TOKEN='…'                             # Jira API token
export LORE_NOTION_TOKEN='…'                           # Notion internal integration secret
export OPENAI_API_KEY='…'                              # the embedder key; the name is fixed
export LORE_LLM_KEY='…'                                # only if you kept the llm: block
```

Notes worth knowing before the first run:

- Every `*_env` key must name a variable that is **set** at load time, or the run
  refuses with e.g. `sources.jira.token_env names LORE_JIRA_TOKEN, but that
  environment variable is not set`.
- `OPENAI_API_KEY` is not configurable — the `openai` embedder reads that exact
  name. `embedder.dimensions` must **not** be set for `openai`; the model implies
  the width.
- Unknown keys are rejected, so a typo in `lore.yaml` fails the load rather than
  being ignored.
- `lore --config path/to/lore.yaml <cmd>` moves the configuration; the default is
  `./lore.yaml`.

The interactive alternative to hand-editing, once the `github:` block is out of
the file:

```bash
lore source add jira
lore source add notion
```

Both prompt for the **name** of the variable holding each credential, never the
credential, and finish with `next: export LORE_JIRA_EMAIL and LORE_JIRA_TOKEN,
then run `lore sync``. They refuse a source that is already configured and tell
you to edit that block directly.

## 4. Sync

```bash
lore sync
```

It streams each source's changes into the index, checkpointing per batch — an
interrupted round resumes where it stopped. Output is one line:

```text
sync complete — `lore status` for counts and cursor ages
```

Pending references are retried at the end of every round, after every source has
run. So even if `OPS-410` was ingested before the Notion page it points at, the
edge forms in the same `lore sync`.

## 5. Status

```bash
lore status
```

```text
documents: 6
chunks:    14
edges:     4

sources:
  jira       last checkpoint 12s ago (2026-09-02T10:04:11Z)
  notion     last checkpoint 11s ago (2026-09-02T10:04:12Z)

sync lock: free
```

*Shape, not a recording.* Fixed by the source: the three count labels and their
alignment, the `<source> last checkpoint <age> (<RFC 3339>)` rows, and
`sync lock: free`. The **numbers are yours**: six documents is three tickets plus
three Notion pages, and the container pages are indexed even though they carry no
text. Chunk and edge counts depend on your body text. A never-synced index prints
`sources: none have checkpointed yet — run `lore sync`` instead of the rows.

The checkpoint age is when lore last *persisted a cursor*, not when the artifact
was last edited.

## 6. Question one — why option B over option A

```bash
lore ask "why did we choose option B instead of A?" --around "incident X" --raw
```

`--around` is what makes this a *provenance* question rather than a search. The
event text is resolved against the index: the earliest document among the top hits
for `"incident X"` becomes the anchor, and the window is `± query.event_window`
(30 days by default) around that document's `created_at`. Here that document is
the incident ticket itself.

`--raw` emits the evidence bundle as JSON and needs no LLM. Piped through `jq`,
the shape is:

```jsonc
{
  "question": "why did we choose option B instead of A?",
  "anchor": {
    "kinds": ["query", "time_window"],
    "query": "why did we choose option B instead of A?",
    "window": {
      "from": "2024-05-04T09:15:00Z",
      "to": "2024-07-03T09:15:00Z",
      "derivation": "event \"incident X\" dated 2024-06-03 via jira:ticket:INC-201",
      "anchored_by": "jira:ticket:INC-201"
    }
  },
  "nodes": [
    {
      "doc": {
        "id": "jira:ticket:ARCH-88",
        "source": "jira",
        "type": "ticket",
        "title": "ARCH-88: Option A versus option B for the checkout write path",
        "author": "Grace Hopper",
        "url": "https://acme-sandbox.atlassian.net/browse/ARCH-88",
        "created_at": "2024-06-05T10:00:00Z",
        "updated_at": "2024-06-06T16:20:00Z"
      },
      "excerpt": "…the chunk that matched…",
      "role": "seed",
      "score": 0.93
    }
    // three more: jira:ticket:INC-201, notion:page:<id>, jira:ticket:OPS-410
  ],
  "chains": [
    ["jira:ticket:OPS-410", "notion:page:<id>", "jira:ticket:INC-201"],
    ["jira:ticket:ARCH-88", "jira:ticket:INC-201"]
  ]
}
```

*Shape, not a recorded transcript.* What the end-to-end suite **asserts** on this
question, and therefore what is guaranteed:

| Guaranteed | Assertion |
| --- | --- |
| the event resolves to a window at all | `bundle.Anchor.Window != nil` |
| the window is anchored on the incident | `window.AnchoredBy == jira:ticket:INC-201` |
| its attribution reads exactly `event "incident X" dated 2024-06-03 via jira:ticket:INC-201` | `window.Derivation` compared verbatim |
| the window is 30 days either side of the incident's `created_at` | `window.From`/`window.To` compared to `±30d` |
| exactly these four documents are cited: `ARCH-88`, `INC-201`, `OPS-410`, the decision page | cited set compared as a sorted set |
| the two chains are exactly `ARCH-88 → INC-201` and `OPS-410 → page → INC-201` | chains compared as sorted text |
| at least one chain runs from Jira into Notion | `assertSpansBothSources` |
| the ticket that rejected option A is cited **and citable** | `ARCH-88` present, `Doc.URL != ""` |
| every node has a URL, no node is cited twice, no chain names an uncited document, and the JSON keeps every anchor string and node URL | `assertBundleContract` |

**Not** guaranteed, so do not read them as promises: the order of `nodes` (ranked
by score, then recency), the order of `chains` (by the best score in each), the
`score` values, the `role` labels — which depend on whether retrieval or the graph
walk reached a document first — and the excerpt text.

### Where the trail ends

`gaps` is **absent** on this corpus, and that is the honest result: all four
documents sit in a chain, and the event resolved. `gaps` is not decoration — it
appears when the engine has something to admit:

| Situation | Gap line |
| --- | --- |
| a retrieved document that nothing links to | `<title> (<doc id>) stands alone; no linked discussion` |
| `--around` text that matches nothing | `could not resolve event "…" to a time — nothing in the index matched the event text` |
| `--around` hits scattered wider than the window | `could not resolve event "…" to a time — candidates: <title> (<date>) <url>; …` |

To *see* a gap, seed the demo but skip the `INC-201` reference in `ARCH-88`: the
ticket still matches the question, now links to nothing, and comes back as a
standalone-seed gap. An unresolvable event is reported the same way — as a gap,
never an error — and the query then runs unwindowed.

### The prose form

Drop `--raw` and `lore ask` synthesizes from the same bundle, with the `llm:`
block doing the writing:

```text
Incident X stalled every checkout write behind one shared row lock [2]. The
options were weighed in ARCH-88, which rejected option A because a hot tenant
still puts two writers on one row and would reproduce the incident [1]. …

**Sources**

1. ARCH-88: Option A versus option B for the checkout write path — https://acme-sandbox.atlassian.net/browse/ARCH-88
2. INC-201: Incident X: checkout writes stalled behind the shared lock — https://acme-sandbox.atlassian.net/browse/INC-201
…
```

The wording is the model's; the structure is not. Every claim carries a bare
`[n]` citation into the numbered evidence, the numbering is the bundle's node
order, and the `**Sources**` list is appended by lore, not by the model. An answer
that cites nothing, cites a number outside the evidence, or writes a citation as a
markdown link is rejected rather than printed.

With no `llm:` block, this same command exits **3** with
`lore: synthesis needs an LLM, and this workspace has no llm: block in lore.yaml
— add one naming the provider, the model and the api_key_env that holds its key`.
`--raw` is the LLM-free path. `lore ask` has no `--explain` flag; prose is its
default.

## 7. Question two — what the decision caused

```bash
lore impact "notion:page:<page id>"
```

The ref may be a document id (as above), a document URL, a ticket key, a commit
SHA, a PR or issue number — or free text, which is interpreted by retrieval. Use
the document id or the URL that `lore ask --raw` reported: a URL matches by exact
equality, so a browser-decorated link may resolve to nothing.

Rendered timeline:

```text
consequences, follow-ups, incidents related to Adopt option B for checkout writes
anchor: Adopt option B for checkout writes
        https://www.notion.so/acme/Adopt-Option-B-d4d4d4d4

2 documents

2024-06-07 Adopt option B for checkout writes
   notion page · 2024-06-07
   https://www.notion.so/acme/Adopt-Option-B-d4d4d4d4
      ## Decision
      We choose option B over option A: every checkout write goes through one owning writer.
      …

2024-06-19 OPS-410: Extend the owning writer to the refunds path
   jira ticket · Katherine Johnson · 2024-06-19 · follow_up
   https://acme-sandbox.atlassian.net/browse/OPS-410
      Follow-up to [the decision page](https://www.notion.so/acme/Adopt-Option-B-d4d4d4d4), which covers the checkout path only.
      …

chains:
  notion:page:d4d4d4d4-0000-4000-8000-000000000004 → jira:ticket:OPS-410
```

*Shape, not a recorded transcript.* Fixed by the renderer: the question line, the
two-line `anchor:` block, the `N documents` count, then one entry per document as
`<created date> <title>` / `<source> <type> · <author> · <date> · <role>` / `<url>`
/ the excerpt indented six spaces. The `role` segment is omitted for `seed`
nodes, and the `author` segment is omitted when there is none — which is why the
Notion page prints `notion page · 2024-06-07` with no name: the Notion connector
does not index page authors.

What the suite **asserts** here:

| Guaranteed | Assertion |
| --- | --- |
| the anchor is the decision page | `bundle.Anchor.Doc.ID == notion:page:<id>` |
| the timeline is exactly the page then `OPS-410`, in that order | cited ids compared as an ordered list |
| entries never go backwards in time | `assertChronological` |
| the follow-up is dated strictly after the anchor | `followUp.Doc.CreatedAt.After(anchor.CreatedAt)` |
| the follow-up is labelled `follow_up` | `Role == entities.RoleFollowUp` |
| the single chain is `page → OPS-410` | chains compared as sorted text |
| that chain crosses from Notion into Jira | `assertSpansBothSources` |
| every node citable, no duplicates, JSON keeps the anchor and URLs | `assertBundleContract` |

Note what `impact` does **not** show: `INC-201` and `ARCH-88` are older than the
page, and the forward walk excludes them by design. Ordering here is
chronological, not by relevance — the opposite of question one.

`--explain` turns this timeline into prose (needs `llm:`), `--raw` emits the same
bundle as JSON, and `--raw` wins if you pass both.

Two more things to try on this workspace:

```bash
lore trace "notion:page:<page id>"   # the same neighborhood, both directions, no time filter
lore why internal/checkout/writer.go:10-20
```

`lore why` refuses, and the refusal is asserted too — exit **3** with
`lore: no repositories registered — code anchoring disabled for this workspace`.
That is the shape of the boundary: an ask-only workspace answers document
questions and declines code questions instead of guessing at them.

## 8. The same walkthrough over MCP

Every verb above is an MCP tool that returns the same `EvidenceBundle` structure —
never prose, so no LLM is involved on this path. Set the server up with
[`quickstart-mcp.md`](quickstart-mcp.md); `lore mcp` speaks MCP on stdin/stdout
for a local harness, and `lore serve` exposes it over HTTP.

| This walkthrough | MCP tool | Arguments |
| --- | --- | --- |
| `lore sync` | `sync_now` | `source` optional — the same filter as `lore sync --source <name>` |
| `lore status` | `sync_status` | — |
| question one | `find_decision` | `question`, `around`, plus optional `source` / `repo` / `doc_type` / `since` / `until` |
| question two | `impact_of` | `ref_or_query`, optional `question` |
| `lore trace` | `trace` | `ref`, optional `direction`, `depth` |
| `lore why` | `why` | `file`, `line_start`, optional `line_end`, `repo`, `question` |
| `lore history` | `history_of` | `path`, optional `repo`, `limit`, `before` |

The tool result is the bundle JSON `lore ask --raw` prints — same field names, same
`chains` and `gaps` — so the assertion tables above describe the MCP answers too.

## 9. Timing and expectations

- **The first sync is the slow one.** It reads every issue with its comments and
  every in-scope page with its block tree, then embeds every chunk. On this
  six-document corpus that is seconds; the cost is dominated by embedder
  round-trips, so a local embedder trades API latency for CPU.
- **Later syncs are incremental.** Each source keeps a watermark and asks only for
  what changed. Re-running `lore sync` on an untouched sandbox does almost nothing.
- **Changing `embedder.model` invalidates the vectors.** `lore sync --reembed`
  rebuilds every chunk and vector; nothing else does.
- **Linking is honest, and honestly sparse.** Edges come from references people
  actually wrote. A real workspace where nobody pastes ticket keys or links yields
  documents that retrieve well and chain to nothing — which shows up as
  `stands alone; no linked discussion` gaps, not as invented connections. This
  demo is seeded densely on purpose; do not read its chain density as typical.
- **The window bounds retrieval, not the walk.** `--around` filters the documents
  retrieval may seed on by `created_at`, and biases ranking toward the window's
  center, so evidence near the event outranks evidence far from it. A document
  reached over a reference edge is cited whichever side of the window it falls on.
  On this corpus the distinction never bites — all four cited documents sit inside
  the 30-day window — so shrink `query.event_window` if you want to watch it
  matter.
- **Counts will not match this page.** Your Notion tree, comment threads and body
  text differ. Only the ids, chains, window attribution and roles are stable.

## 10. Recording the demo

Producing the asciinema cast or the GIF is **a human step**. This repository ships
no recording automation, and nothing here writes to Jira or Notion — a recording
runs against a sandbox you seeded yourself.

Sequence to record, after seeding and configuring, so the cast shows a cold index
becoming an answered question:

```bash
rm -f ~/.lore/lore-askonly.db          # start from a cold index
lore status                            # zeros: "none have checkpointed yet"
lore sync                              # the one slow beat
lore status                            # counts and fresh cursors
lore ask "why did we choose option B instead of A?" --around "incident X" --raw | jq
lore ask "why did we choose option B instead of A?" --around "incident X"
lore impact "notion:page:<page id>"
lore why internal/checkout/writer.go:10-20   # the refusal: exit 3
```

Practical notes for a clean take: run it in a shell whose history and prompt do
not leak anything, keep the token exports in a sourced file **outside** the
recording, and remember that the `--raw | jq` frame is the one worth pausing on —
it is the only frame that shows the window attribution and the chains together.

## 11. Teardown

The index is derived data. Deleting it loses nothing:

```bash
rm -f ~/.lore/lore-askonly.db
```

The next `lore sync` rebuilds it from the sources. Nothing was ever written to
Jira or Notion, so tearing the demo down is deleting that file, your `lore.yaml`,
and — at your leisure — the sandbox artifacts you created by hand.
