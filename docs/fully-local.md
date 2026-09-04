# Fully local

Lore's retrieval, graph and storage layers are in-process: the index is a single SQLite
file and search runs inside the binary. Only two components ever talk to a model — the
**embedder** (always) and the **LLM** (only for synthesis). Point both at a local Ollama
daemon and nothing about your project leaves the machine.

## What leaves the machine

`embedder` is mandatory: every chunk is embedded at sync, and every question is embedded
at query time. `llm:` is optional; without it, prose synthesis is the only thing that
fails, and it says so.

| Mode | What leaves | To which endpoint | What stays |
|---|---|---|---|
| `embedder: openai` + `llm: openai\|anthropic\|openai-compatible` | Chunk text at sync (batched, ≤512 inputs per request) and question text at query; then the whole evidence bundle for synthesis — question, anchor summary, and per cited document its title, source, type, role, author, creation time, URL and excerpt, plus the provenance chain lines and gap list | `https://api.openai.com/v1/embeddings` for vectors; synthesis to `https://api.openai.com/v1/chat/completions`, `https://api.anthropic.com/v1/messages`, or the chat URL of the `openai-compatible` preset you bound — `https://api.z.ai/api/paas/v4/chat/completions` for `preset: zai` | Index file, BM25 lexical search, vector KNN, graph walk, `git blame`/`git log` |
| `embedder: openai`, no `llm:` block | Chunk text at sync, question text at query. Nothing else — no evidence bundle, no document bodies | `https://api.openai.com/v1/embeddings` only | Everything above, and every answer: `lore ask --raw`, and `why`/`trace`/`impact`/`history` without `--explain`, return the evidence bundle without contacting an LLM |
| `embedder: ollama`, no `llm:` block | Nothing leaves the machine (loopback only) | `http://127.0.0.1:11434/api/embed` | Everything |
| `embedder: ollama` + `llm.provider: ollama` | Nothing leaves the machine (loopback only) | `/api/embed` and `/api/chat` on `http://127.0.0.1:11434` | Everything, synthesis included |

Two verbs never touch the embedder at all: `lore trace` resolves a ref and walks edges,
and `lore history` reads `git log` and walks edges. Their retrieval is local under every
mode; only `--explain` on top of them reaches the configured LLM.

**Source connectors are inbound-only reads.** Nothing in Lore writes to a hosted service.
A sync sends the configured token and query parameters, and reads pages of results back:

| Source plugin | Endpoint | Direction |
|---|---|---|
| `use: github` | `https://api.github.com` | read only |
| `use: notion` | `https://api.notion.com` | read only |
| `use: jira` | that instance's `with.base_url` | read only |
| `use: gitlab` | that instance's `with.base_url` (default `https://gitlab.com`) | read only |

Source traffic is orthogonal to the model traffic above: an Ollama-only workspace still
reaches GitHub to *ingest*, and an air-gapped workspace with zero sources is valid — set
`repos:` and index nothing but local clones. See [`sources.md`](sources.md).

**MCP needs no LLM.** The MCP tools (`find_decision`, `why`, `trace`, `impact_of`,
`history_of`, `sync_now`, `sync_status`) return an `EvidenceBundle` and never call a
completion API — the host model is already the LLM. The embedder is still used for the
searching tools, so the fully-private MCP setup is `embedder.provider: ollama` with no
`llm:` block at all. See [`quickstart-mcp.md`](quickstart-mcp.md).

## Setup

Install the daemon and pull one embedding model and one chat model:

```
brew install ollama          # or: curl -fsSL https://ollama.com/install.sh | sh
ollama serve                 # listens on 127.0.0.1:11434
ollama pull nomic-embed-text # embeddings
ollama pull llama3.1:8b      # synthesis
```

The vector width is **configured, never probed** — Lore has to know its embedder identity
without the daemon running, and the request it sends carries no `dimensions` field. Read
the width off the model and copy it into `lore.yaml`:

```
ollama show nomic-embed-text
```

Use the embedding length that command reports (`768` for `nomic-embed-text`; confirm on
your own machine, since a re-tagged model can differ):

```yaml
embedder:
  provider: ollama            # a providers[] id, or a provider plugin used with its defaults
  model: nomic-embed-text
  dimensions: 768             # what `ollama show nomic-embed-text` reports

llm:
  provider: ollama
  model: llama3.1:8b

# providers:                              # optional: omit it and `ollama` runs on its defaults
#   - use: ollama
#     with:
#       base_url: http://127.0.0.1:11434  # this is the default
# no api_key_env anywhere: the ollama plugin declares no secret, so Lore never reads a key for it
```

`lore init` writes `llm:` commented out, so a fresh workspace has no synthesis until you
add that block by hand.

Two rules to keep straight, because they are opposite per provider:

- `dimensions` **required** for `ollama`. Omit it and startup fails with:
  > ollama: embedder.dimensions must be set to the vector width of nomic-embed-text: an Ollama model does not imply one; `ollama show nomic-embed-text` reports it
- `dimensions` **forbidden** for `openai`, where the model implies the width:
  > openai: embedder.dimensions must not be set for this provider: the vector width follows from embedder.model

If the number is wrong, the first embed call catches it rather than storing garbage:
`ollama: model "nomic-embed-text" returned 768 dimensions for input 0, want 1024`.

A provider name the build does not have is refused at startup too. Providers are plugins,
and each manifest declares the capabilities it serves, so a role bound to a provider that
does not serve it is refused just as hard — `embedder binds provider "anthropic", which
does not serve embed; it serves complete`. What this build ships:

| Provider plugin | Serves | Credential | Endpoint |
|---|---|---|---|
| `openai` | embeddings, completions | `api_key_env`, defaulting to `OPENAI_API_KEY` | `https://api.openai.com` + `/v1/embeddings`, `/v1/chat/completions` |
| `anthropic` | completions | `api_key_env`, defaulting to `ANTHROPIC_API_KEY` | `https://api.anthropic.com/v1/messages` |
| `ollama` | embeddings, completions | none — the daemon is unauthenticated | `http://127.0.0.1:11434` + `/api/embed`, `/api/chat` |
| `openai-compatible` | embeddings, completions — per preset, see below | `api_key_env`, no default: every vendor names its own variable | the preset's base URL + its paths |

`zai` is no longer a provider name. Z.AI is one preset of the `openai-compatible` driver,
alongside the vendors it also reaches:

| `preset` | Base URL | Chat path | Embeddings path |
|---|---|---|---|
| `zai` | `https://api.z.ai/api` | `/paas/v4/chat/completions` | none published |
| `openrouter` | `https://openrouter.ai/api` | `/v1/chat/completions` | `/v1/embeddings` |
| `moonshot` | `https://api.moonshot.ai` | `/v1/chat/completions` | none published |
| `deepseek` | `https://api.deepseek.com` | `/chat/completions` | none published |
| `groq` | `https://api.groq.com/openai` | `/v1/chat/completions` | none published |
| `together` | `https://api.together.ai` | `/v1/chat/completions` | `/v1/embeddings` |
| `vllm` | `http://localhost:8000` | `/v1/chat/completions` | `/v1/embeddings` |
| `lmstudio` | `http://localhost:1234` | `/v1/chat/completions` | `/v1/embeddings` |

A row with no published embeddings endpoint refuses an `embedder:` binding by name rather
than guessing a path: `openai-compatible: preset zai serves no OpenAI-compatible
embeddings endpoint; bind embedder to a provider that does, or set embeddings_path if this
vendor has since published one`. So Z.AI as the synthesis half is:

```yaml
providers:
  - id: zai
    use: openai-compatible
    with:
      preset: zai
      api_key_env: LORE_ZAI_KEY

llm:
  provider: zai
  model: glm-4.6
```

The last two rows are self-hosted, and their default base URLs are loopback, so a `vllm`
or `lmstudio` preset left at its default reaches nothing off the machine — the same
property the `ollama` rows in the table above have. `base_url` overrides the port, or the
host, when you serve them elsewhere.

## Remote but private

`base_url` is a key on the provider instance, and it accepts any host, so a shared GPU box
works. One instance serves both roles here — an embedding connection and a chat connection
are built separately from it, but they are configured once:

```yaml
providers:
  - use: ollama
    with:
      base_url: http://gpu-box.internal:11434

embedder:
  provider: ollama
  model: nomic-embed-text
  dimensions: 768

llm:
  provider: ollama
  model: llama3.1:8b
```

Be honest with yourself about what this changes. The privacy claim becomes "nothing leaves
my network", not "nothing leaves my machine". Chunk text, question text and — with `llm:`
set — the full evidence bundle now cross the wire, and the Ollama daemon is
**unauthenticated and plain HTTP**: Lore sends no credential to it, so anything that can
reach that port can use that model and read those requests. Treat the daemon as a private
service behind a network boundary or a TLS-terminating proxy; `base_url` will take an
`https://` URL.

The same key is the escape hatch for an OpenAI-protocol gateway inside your own
perimeter: an `openai` instance carrying `base_url: https://gateway.internal` sends
vectors to your gateway, not to `api.openai.com`. The API key still comes from
`OPENAI_API_KEY`:

```yaml
providers:
  - id: gateway
    use: openai
    with:
      base_url: https://gateway.internal

embedder:
  provider: gateway
  model: text-embedding-3-small
```

## Switching an existing workspace to Ollama

The embedder identity is `plugin/model/dimensions` — the provider *plugin*, not the
instance id you bound, so renaming an instance does not invalidate an index. It is
recorded in the index's `meta`
table under `embedder_identity` at the first sync. Vectors from one embedder are
meaningless to another, so the next sync refuses to mix them:

> embedder identity mismatch: this index was built with "openai/text-embedding-3-small/1536" but the workspace is now configured for "ollama/nomic-embed-text/768" — vectors from one embedder are meaningless to another, so run `lore sync --reembed` to wipe the chunk layer and rebuild it with "ollama/nomic-embed-text/768"

`lore sync --reembed` rewinds every cursor, wipes the chunk layer and re-reads every
source from the beginning. `lore sync --source <instance>` exists for ordinary sync rounds,
but combining it with `--reembed` is refused: "cannot re-embed a single source: a
re-embed wipes every source's chunks and rewinds every cursor, so it must run across the
whole workspace". A re-embed is always workspace-wide.

**The width is a separate, harder constraint.** The vector table's dimension count is
baked into its declaration when the file is created, and `--reembed` deletes rows without
redeclaring it. So if the new embedder has a *different* width — `1536` → `768` is the
common case — the process never gets as far as the identity check; opening the index fails
first:

```
lore: cannot open the workspace index at ~/.lore/myproject.db: sqlite: ~/.lore/myproject.db: database vector_dims is "1536", this store expects "768"
```

No sync runs, `--reembed` included. The index is derived data; delete it and sync from
scratch:

```
rm ~/.lore/myproject.db          # or whatever index_path resolves to
lore sync
```

Use `--reembed` when the width is unchanged (same-width model swap, or a re-chunk), and
delete the file when the width changes.

## Cost and quality tradeoffs

Local costs nothing per token and leaks nothing. What you give up is retrieval precision:
locally-runnable embedding models are generally weaker than the hosted ones, and in Lore
that shows up specifically as looser semantic recall — the vector half of the hybrid
search surfaces less relevant chunks, so the seed set the graph walk starts from is
noisier.

What does **not** change:

- the BM25 lexical half of the search, which is FTS5 inside the index and never sees a model;
- the provenance graph walk, edge resolution and chain assembly, which run over stored edges;
- `git blame`/`git log` anchoring for `why` and `history`;
- the citations. Every answer still points at real document URLs, so a weak retrieval is
  visible and checkable rather than silent.

Synthesis quality is a separate axis: the synthesis prompt forbids the model from adding
facts of its own and requires an inline citation per claim, and an answer citing evidence
that is not in the bundle is rejected. A smaller local chat model is more likely to be
*refused* than to hallucinate.

## Verification checklist

```
# 1. daemon reachable, models present
curl -fsS http://127.0.0.1:11434/api/tags
ollama list

# 2. width matches the config
ollama show nomic-embed-text

# 3. index builds — prints "sync complete — `lore status` for counts and cursor ages"
lore sync

# 4. counts are non-zero and the lock is free
lore status
#   documents: 412
#   chunks:    1930
#   edges:     880
#
#   sources:
#     github     last checkpoint 2m ago (2026-09-02T10:14:00Z)
#
#   sync lock: free

# 5. retrieval works without an LLM
lore ask --raw "why did we pick sqlite?"

# 6. synthesis works locally (needs the llm: block)
lore ask "why did we pick sqlite?"
```

If step 5 succeeds and step 6 fails, the embedder is fine and the problem is the `llm:`
block. A workspace with no `llm:` block answers with:

> synthesis needs an LLM, and this workspace has no llm: block in lore.yaml — add one naming the provider, the model and the api_key_env that holds its key

The `api_key_env` that remedy names is a key on the provider instance rather than on the
`llm:` block, and the `ollama` plugin declares no secret at all, so an Ollama synthesis
block needs only a provider and a model.

The first Ollama call after a `pull` loads the model, which can take tens of seconds; the
embedder allows 120s per request and the chat client the same, and both retry a 429, a 5xx
or a transport error up to 4 attempts with jittered backoff.

## See also

- [`sources.md`](sources.md) — what each connector ingests and the tokens it needs
- [`quickstart-mcp.md`](quickstart-mcp.md) — wiring Lore into an MCP host, which needs no LLM
- [`v3/06-interfaces-and-config.md#security-posture`](v3/06-interfaces-and-config.md#security-posture) — the full posture: read-only sources, env-only secrets, loopback/TLS rules
