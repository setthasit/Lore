# MCP quickstart — Claude Code and Cursor

Lore speaks MCP over two transports: `lore mcp` (stdio) and `lore serve` (streamable
HTTP at `/mcp`). Both advertise the same seven tools from the same server registration,
so a config that works on one works on the other.

## Why MCP is the cheapest way to run Lore

Every query tool returns a structured `EvidenceBundle` — cited documents with an
excerpt, a role, a score, the edges they were reached through, and the URL each came
from. Nothing is synthesized server-side; the tool descriptions say so explicitly
("Returns an evidence bundle, not an answer … Nothing is synthesized here — you write
the explanation from these citations"). The host model in Claude Code or Cursor is
already an LLM, so it does the writing in its own context and can immediately call
another tool with what it just learned.

Consequence: **MCP usage needs no `llm:` block and no LLM API key.** A workspace with no
`llm:` block resolves to a nil LLM and only the synthesis path — `lore ask` and
`--explain` on the CLI — fails, with a message that says what to add. `lore mcp` and the
MCP half of `lore serve` never touch it. `lore init` writes the `llm:` block commented
out, so a fresh workspace is already in exactly this state.

You still need an **embedding** key (or a local Ollama embedder — see
[fully-local.md](fully-local.md)): retrieval embeds your question at query time.

| Surface | Embedder | LLM |
|---|---|---|
| MCP tools (`lore mcp`, `lore serve`) | required | ✅ not needed |
| `lore ask`, `--explain` | required | required |

## Prerequisites

```bash
go install github.com/setthasit/Lore/cmd/lore@latest   # → $GOBIN/lore
# or, from a checkout:
make bin                             # stamped build at bin/lore (CGO_ENABLED=0)
```

Then, in the workspace directory:

```bash
lore init                            # writes ./lore.yaml
export LORE_GITHUB_TOKEN=...         # the variable lore.yaml names
export OPENAI_API_KEY=...            # the embedder key
lore sync                            # first run creates the index
lore status                          # counts, per-source cursor ages, lock state
```

Four things must be true before a host model gets useful answers:

1. **A binary you can name by absolute path.** MCP hosts spawn the command themselves;
   they do not run it through your shell's `PATH` resolution rules or your rc files.
2. **A `lore.yaml`.** Every command takes `--config`, defaulting to `./lore.yaml`. If it
   is missing the process exits with
   ``no configuration at ./lore.yaml — run `lore init` to create one``.
   Minimum for an MCP-only workspace: a `workspace` name, at least one source (or one
   entry under `repos:`), and an `embedder` block.

   ```yaml
   workspace: myproject
   sources:
     - use: github
       with:
         token_env: LORE_GITHUB_TOKEN
         repos:
           - acme/myproject
   repos: []                          # local clones, for `why` and `history_of` only
   embedder:
     provider: openai                 # key comes from OPENAI_API_KEY
     model: text-embedding-3-small    # do not set embedder.dimensions for openai
   ```

   Unknown keys are rejected at load, so a typo is a startup error rather than a silent
   default. The full source list and each block's rules live in
   [sources.md](sources.md) and
   [docs/v3/06-interfaces-and-config.md](v3/06-interfaces-and-config.md).
3. **The environment variables that `lore.yaml` names.** Startup requires the named
   variable to actually be set: a missing one fails with
   `sources[github].with.token_env names LORE_GITHUB_TOKEN, but that environment variable is not set`.
4. **At least one completed `lore sync`.** Tools read the index; an unsynced workspace
   answers every question with an empty bundle. `lore sync` takes `--source <instance>` to
   sync just one connector (an unknown name is refused with
   `unknown source "<name>"; this workspace has <configured names>`) and `--reembed` to
   rebuild every chunk and vector — the two cannot be combined, since a re-embed always
   runs across the whole workspace. The MCP `sync_now` tool's `source` field is the same
   filter.

## Transport 1 — stdio (`lore mcp`)

`lore mcp` speaks MCP on stdin/stdout and returns when the client disconnects or the
process is interrupted. **Nothing else may be printed on stdout while it runs: the
stream is the protocol.** Diagnostics go to stderr for exactly this reason, which is
also where the host client will show you startup failures.

### Claude Code

```json
{
  "mcpServers": {
    "lore": {
      "command": "/absolute/path/to/lore",
      "args": ["mcp", "--config", "/absolute/path/to/lore.yaml"],
      "env": {
        "OPENAI_API_KEY": "<your-embedding-api-key>",
        "LORE_GITHUB_TOKEN": "<your-read-only-github-token>"
      }
    }
  }
}
```

Claude Code reads this shape from a user-level `~/.claude.json` and from a
project-level `.mcp.json` committed at the repository root; `claude mcp add` writes it
for you. **The exact files and precedence are Claude Code's contract, not Lore's, and
cannot be verified from this repository** — check
<https://code.claude.com/docs/en/mcp-quickstart> before hand-editing, and prefer
`claude mcp add` so the client picks the location. If you do commit `.mcp.json`, keep
the `env` values out of it and export them in the environment that launches the client
instead.

### Cursor

Same server object under the same `mcpServers` key, in Cursor's `mcp.json`:

```json
{
  "mcpServers": {
    "lore": {
      "command": "/absolute/path/to/lore",
      "args": ["mcp", "--config", "/absolute/path/to/lore.yaml"],
      "env": {
        "OPENAI_API_KEY": "<your-embedding-api-key>",
        "LORE_GITHUB_TOKEN": "<your-read-only-github-token>"
      }
    }
  }
}
```

Cursor's on-disk locations (project `.cursor/mcp.json` vs. a global one under your home
directory) are likewise **not verifiable from this repository** — see
<https://cursor.com/docs/mcp>.

### Two things that bite everyone

- **Absolute paths, always.** `--config` is resolved relative to the *client's* working
  directory, which is rarely your workspace. Pass it explicitly. The relative default
  `./lore.yaml` only makes sense when you run `lore` yourself.
- **The spawned environment is not your shell.** A GUI-launched client typically
  inherits the desktop session's environment, not the one your `.zshrc` exports, so
  `OPENAI_API_KEY` and every `*_env` variable your `lore.yaml` names can be absent even
  though `lore sync` works fine in your terminal. That is what the `env` block above is
  for. Lore reads secrets from the environment only — it never stores them in
  `lore.yaml`.

## Transport 2 — streamable HTTP (`lore serve`)

```bash
lore serve --http 127.0.0.1:8080
```

```
lore: serving MCP on http://127.0.0.1:8080/mcp
lore: serving gRPC on 127.0.0.1:9090
```

Both lines go to stderr. `serve` runs three things at once: the MCP endpoint, the
`lore.v1` gRPC API, and the sync scheduler (`scheduler.interval`, default `30m`), so the
index stays fresh while the endpoint is up.

- **The endpoint is exactly `/mcp`.** Nothing else is served: `/`, `/mcp/`,
  `/mcp/extra` and `/metrics` all return `404`.
- **There is no default HTTP address.** Without `--http` or `server.http_addr`, `serve`
  refuses to start:
  `no address to serve on: set server.http_addr in lore.yaml or pass --http 127.0.0.1:8080`.
  (gRPC does default, to `127.0.0.1:9090`.)
- **Sessions are stateless.** The handler rejects every server-to-client request,
  because no tool here makes one. At shutdown, in-flight tool calls get a 5-second grace
  window to finish answering.

Client config — the same `mcpServers` key with a URL instead of a command:

```json
{
  "mcpServers": {
    "lore": {
      "url": "http://127.0.0.1:8080/mcp"
    }
  }
}
```

Here the server's environment is the shell you ran `lore serve` in, so there is no `env`
block to get wrong.

### Off-loopback requires TLS — the exact rule

An address is served in the clear only if it is *provably* loopback: the host must parse
as an IP address that is loopback. A host name is never proof (it is not resolved), and
an empty host as in `:8080` reaches every interface. Anything else is refused unless
both `server.mtls.cert` and `server.mtls.key` are set. The refusal, verbatim, for
`lore serve --http 0.0.0.0:8080`:

```
server.http_addr "0.0.0.0:8080" is not a loopback address, so it must be served over TLS: set both server.mtls.cert and server.mtls.key, or bind 127.0.0.1:8080
```

So `:8080` and `localhost:8080` are both rejected; `127.0.0.1:8080` is accepted. The
same check runs against the gRPC address under the setting name `server.grpc_addr`.

With `server.mtls.cert` and `server.mtls.key` configured, the MCP endpoint is served
over TLS 1.3 and the startup line reads `https://…/mcp` — point the client at the
`https` URL. Setting only one of the two is an error:

```
server.mtls needs both cert and key to serve over TLS: set both, or remove the block to serve in the clear
```

`--mtls` and `server.mtls.client_ca` add *client*-certificate verification to the gRPC
listener only; the MCP endpoint is not mutually authenticated.

## Tool routing

All seven tools are registered identically on both transports. The five query tools
carry `readOnlyHint`; `sync_now` is the only tool that writes anything, and what it
writes is the local index — never a source.

| Tool | Answers | Input fields |
|---|---|---|
| `find_decision` | "why B over A?" — breadth across a decision, from the index; works with zero repos | `question` (required); optional `around`, `source`, `repo`, `doc_type`, `since`, `until` |
| `why` | "why do these lines look like this?" — blame-anchored on a registered local clone | `file`, `line_start` (required); optional `repo`, `line_end`, `question` |
| `trace` | depth on one document: what it came from, what came out of it, plus its full body | `ref` (required — SHA, PR/issue number, ticket key, URL or DocID); optional `direction` (`out`/`in`/`both`), `depth` |
| `impact_of` | what followed a decision, chronologically | `ref_or_query` (required — a ref, or free text naming the decision); optional `question` |
| `history_of` | how one whole file evolved, newest commits first, one page at a time | `path` (required); optional `repo`, `limit`, `before` |
| `sync_now` | refresh the index now and answer when the round finishes | optional `source` (omit to sync every configured source) |
| `sync_status` | how much is stored, how stale each source is, whether a round is writing | none |

Notes worth knowing before the model guesses:

- `since`/`until` on `find_decision` take `YYYY-MM-DD` (from its first instant / through
  its last) or an RFC 3339 timestamp. Anything else:
  `since: "last tuesday" is neither a date (YYYY-MM-DD) nor an RFC 3339 timestamp`.
- `why` and `history_of` need the file's repository registered under `repos:` as a local
  clone. A zero-repo workspace cannot anchor on code at all — see troubleshooting.
- `history_of` pages backwards through `before`: pass the **last entry of
  `anchor.code.blamed_shas`** from the previous bundle, never a node id (nodes are
  sorted oldest-first and their ids are document ids, not commit SHAs). An empty
  `blamed_shas` means the history is exhausted.
- `sync_status` reports ages in whole seconds counted at call time, never wall-clock
  timestamps, so a model can judge staleness without knowing the current time. A source
  missing from `sources` has never been synced.
- An empty bundle is a real answer, not a failure: the index holds no evidence for that
  question.

## A worked agent session

> **Illustrative shape, not a recorded run.** The field names, tool names and argument
> names below are the real wire contract; the document ids, titles, URLs, scores and
> excerpt text are invented, and every bundle is abbreviated — real ones carry up to
> `query.top_k` nodes (default 12) with full excerpts. Run it against your own workspace
> to see real values.

**User:** "Why are we on SQLite instead of Postgres, and what did that decision cost us
later?"

**Step 1 — breadth.** The model has no file or line to point at, so it starts with
`find_decision`:

```json
{ "question": "why SQLite instead of Postgres", "since": "2025-01-01" }
```

```json
{
  "question": "why SQLite instead of Postgres",
  "anchor": { "kinds": ["query"], "query": "why SQLite instead of Postgres" },
  "nodes": [
    {
      "doc": {
        "id": "notion:page:design/storage",
        "source": "notion", "type": "page",
        "title": "Storage design",
        "url": "https://notion.so/design/storage",
        "created_at": "2025-03-10T09:12:00Z", "updated_at": "2025-03-11T17:40:00Z"
      },
      "excerpt": "postgres with pgvector was the alternative …",
      "role": "design_doc", "score": 0.81
    },
    {
      "doc": {
        "id": "github:pr:acme/myproject/pull/12",
        "source": "github", "type": "pr",
        "title": "Index on SQLite, not Postgres",
        "url": "https://github.com/acme/myproject/pull/12",
        "created_at": "2025-03-12T11:02:00Z", "updated_at": "2025-03-12T15:31:00Z"
      },
      "excerpt": "sqlite ships everywhere and needs no server …",
      "role": "semantic_match", "score": 0.77,
      "via": [{ "src": "notion:page:design/storage", "dst": "github:pr:acme/myproject/pull/12",
                "kind": "references_doc", "confidence": 0.9 }]
    }
  ],
  "chains": [["notion:page:design/storage", "github:pr:acme/myproject/pull/12"]]
}
```

`chains` and `gaps` are `omitempty` on the wire: this bundle has a `chains` entry but
no gap, so `gaps` is absent rather than sent as `[]` — an empty `chains` would be
dropped the same way.

**Step 2 — depth on the decisive document.** The PR is where the choice landed, so the
model asks for its neighborhood and full body:

```json
{ "ref": "github:pr:acme/myproject/pull/12", "direction": "both" }
```

The bundle comes back with `anchor.kinds: ["document"]`, `anchor.doc` naming that PR,
the anchor node carrying `role: "seed"` and its **whole text** instead of an excerpt,
neighbors reached through `commit_in_pr` and `pr_closes_issue` edges, the `chains` that
connect them, and a `gaps` entry for every trail that dead-ends — for example
`PROJ-4521 (jira:ticket:PROJ-4521) stands alone; no linked discussion`.

**Step 3 — consequences.** "What did it cost us later" is `impact_of`, anchored on the
same document:

```json
{ "ref_or_query": "github:pr:acme/myproject/pull/12", "question": "operational cost" }
```

Everything reached is reported as a consequence in chronological order, each node with
`role: "follow_up"`. If nothing followed, the bundle says so explicitly rather than
returning nothing useful — `gaps: ["no follow-up evidence after 2025-03-12"]`.

**What the host model then writes** is prose the *client* produced, not Lore output:

> You chose SQLite in [Storage design](https://notion.so/design/storage) (2025-03-10),
> where Postgres with pgvector was the alternative, and landed it in
> [#12](https://github.com/acme/myproject/pull/12) (2025-03-12) on the grounds that
> SQLite ships everywhere and needs no server. Two follow-ups came out of it … One trail
> ends unexplained: PROJ-4521 has no linked discussion.

Every claim points at a URL that came out of a bundle. That property is the whole point
of the tool surface returning evidence instead of prose.

## Troubleshooting

| Symptom | What you see | Fix |
|---|---|---|
| Server never appears in the client's tool list | The client shows the server as failed; the real reason is on the process's **stderr** — most often ``no configuration at ./lore.yaml — run `lore init` to create one`` (relative `--config`, or none), `embedder.provider.with.api_key_env names OPENAI_API_KEY, but that environment variable is not set`, or `sources[github].with.token_env names LORE_GITHUB_TOKEN, but that environment variable is not set` | Pass an absolute `--config`, and supply every variable your `lore.yaml` names through the client's `env` block. Verify the command outside the client first: `/absolute/path/to/lore mcp --config /absolute/path/to/lore.yaml` should sit there silently instead of exiting. A connected server identifies itself as `lore` …
| `why` or `history_of` fails on a zero-repo workspace | `no repositories registered — code anchoring disabled for this workspace` | Blame and file history need a local clone. Add one under `repos:` (`path:`, plus `remote:` to map it onto an ingested source repo), or ask `find_decision` instead — it answers the same question from the index. Naming an unregistered repo instead returns `repo "…" is not registered — registered repos: …` |
| `sync_now` refuses to run | `cannot run a sync round — myhost/4213 (last heartbeat 3s ago) is already writing this index; retry later, or wait out the 60s lease TTL if that holder crashed` | Something else holds the workspace lease — usually a manual `lore sync`, or the scheduler inside a running `lore serve`. Rounds are exclusive across every process sharing the workspace, and a refused round writes nothing. Report it and wait; do not retry in a loop. `sync_status` shows `sync_lock.held`, `holder`, `held_for_seconds` and `last_heartbeat_seconds_ago` — a heartbeat many minutes old means that holder most likely died, and the next round takes the lock over |
| `sync_now` refuses after an embedder change | ``embedder identity mismatch: this index was built with "openai/text-embedding-3-small/1536" but the workspace is now configured for "ollama/nomic-embed-text/768" — vectors from one embedder are meaningless to another, so run `lore sync --reembed` to wipe the chunk layer and rebuild it with "ollama/nomic-embed-text/768"`` | Identity is `plugin/model/dimensions` — the provider plugin, not the instance id — so changing any one of the three invalidates every stored vector. Run `lore sync --reembed` once from the terminal. Note the index file's vector width is fixed when it is created, so a change of width also needs a fresh index path — the index is derived data, safe to delete |
| Embedder block rejected at startup | `openai: embedder.dimensions must not be set for this provider: the vector width follows from embedder.model`, or ``ollama: embedder.dimensions must be set to the vector width of nomic-embed-text: an Ollama model does not imply one; `ollama show nomic-embed-text` reports it`` | The rule is inverted per provider plugin: forbidden for `openai`, required for `ollama`. An unknown OpenAI model reports `openai: embedder.model … has no known vector width; supported models: …`, and a role bound to a provider that does not serve it reports `embedder binds provider "anthropic", which does not serve embed; it serves complete` |
| HTTP transport won't start | `no address to serve on: set server.http_addr in lore.yaml or pass --http 127.0.0.1:8080`, or the loopback/TLS refusal quoted above | Bind a literal loopback IP, or configure `server.mtls.cert` and `server.mtls.key` and use the `https` URL |
| Tools connect but every bundle is empty | No error — an empty bundle is a valid answer | Call `sync_status`: a source that last checkpointed days ago cannot hold what happened yesterday, and a source absent from `sources` has never synced. Then `sync_now`. If it is still empty, widen the filters or rephrase — `source`, `repo`, `doc_type`, `since` and `until` are all hard filters |
| Session dies mid-stream on stdio | Garbled JSON-RPC in the client log | Anything on stdout corrupts the protocol. Lore keeps its own diagnostics on stderr; make sure no wrapper script of yours echoes to stdout |

## See also

- [sources.md](sources.md) — every source block, its keys and the token scopes it needs.
- [fully-local.md](fully-local.md) — Ollama embedder, no cloud provider in the path.
- [docs/v3/06-interfaces-and-config.md](v3/06-interfaces-and-config.md) — the tool
  surface, the full `lore.yaml` reference and the gRPC API.
