# 06 — Interfaces & Config

## MCP (stdio + Streamable HTTP)

Implemented with the official Go MCP SDK
([modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk)).
All tools return structured `EvidenceBundle` JSON (never synthesized prose —
[02 — D1](02-architecture.md#key-design-decisions)). All query tools carry
`readOnlyHint`; only `sync_now` mutates local state (the index — never any
source).

| Tool | Input | Returns |
|---|---|---|
| `find_decision` | `question`, optional `around` (event text or date), `source`, `repo`, `doc_type`, `since`, `until` | EvidenceBundle seeded by retrieval; works with zero repos |
| `why` | `repo`, `file`, `line_start`, `line_end`, optional `question` | EvidenceBundle anchored on blame; precondition error if no repos registered |
| `trace` | `ref` (SHA / PR# / ticket key / URL / DocID), optional `direction`, `depth` | full provenance neighborhood + body, chronological |
| `impact_of` | `ref_or_query`, optional `question` | chronological impact timeline after the anchor decision |
| `history_of` | `path`, optional `repo`, `limit`, `before` | chronological file timeline; precondition error if no repos registered |
| `sync_now` | optional `source` | acknowledgment; errors if lock held |
| `sync_status` | — | last run per connector, cursor ages, doc/edge counts, lock state |

Tool descriptions (in the MCP schema) spell out the division of labor so host
models route well: *breadth* → `find_decision`/`why`; *depth on one node* → `trace`;
*consequences* → `impact_of`; *file evolution* → `history_of`.

Tool-surface policy: `EvidenceBundle` is the stable contract
([02 — D9](02-architecture.md#key-design-decisions)); tools are cheap verbs.
Deprecation happens in place — a deprecated tool keeps its registration with
a "deprecated — use X" description for one release before removal.

Transports:

- `lore mcp` — stdio, for local agent harnesses (Claude Code, Cursor, …).
- `lore serve` — Streamable HTTP endpoint (`/mcp`) alongside gRPC.

## CLI

```
lore init                          # create workspace + lore.yaml scaffold
lore source add github|notion|jira # append source config interactively
lore sync [--source=jira] [--reembed]
lore status                        # sync state, doc/edge counts, lock state
lore ask "<question>" [--around="incident X"] [--since --until] [--source --repo --type]   # → find_decision
lore impact <ref | "query"> [--question="…"]
lore why <file>:<L1>-<L2> ["question"] [--repo=…]
lore trace <ref> [--direction=in|out|both]
lore history <path>
lore mcp                           # MCP stdio server
lore serve [--http=:8080] [--grpc=:9090] [--mtls]
```

Human-facing defaults: `why`/`trace`/`impact`/`history` pretty-print the
evidence chain (tree/timeline with URLs); `lore ask` and `--explain` invoke
SynthesisService (requires LLM config); `--raw` emits the bundle as JSON for
scripting.

## gRPC — programmatic API (`lore.v1`)

Not an MCP transport ([02 — D5](02-architecture.md#key-design-decisions)).
Exists for programmatic consumers, primarily the future web UI.

```proto
service QueryService {
  rpc FindDecision(FindDecisionRequest) returns (FindDecisionResponse);
  rpc Why(WhyRequest) returns (WhyResponse);
  rpc Trace(TraceRequest) returns (TraceResponse);
  rpc ImpactOf(ImpactOfRequest) returns (ImpactOfResponse);
  rpc HistoryOf(HistoryOfRequest) returns (HistoryOfResponse);
}

service SyncService {
  rpc Trigger(TriggerRequest) returns (TriggerResponse);   // errors if lock held
  rpc Status(StatusRequest) returns (StatusResponse);
  rpc Watch(WatchRequest) returns (stream SyncEvent);      // live progress: per-connector
}                                                          // phase, counts, errors
```

- Every query request has `bool synthesize` (default true on this surface);
  responses carry the raw `EvidenceBundle` **and** optional `synthesis` text —
  the web UI renders the provenance graph from the bundle and prose from the
  synthesis.
- `SyncService.Watch` streams sync progress — needed for a UI progress view.

### mTLS

- `lore serve --mtls`: TLS listener with **required client-certificate
  verification** against a configured client CA (`tls.RequireAndVerifyClientCert`).
- Config: server cert/key + client CA bundle paths in `lore.yaml`; a
  `make certs.dev` target generates a local CA + server/client certs for
  development.
- Plain-TCP gRPC allowed only on loopback; non-loopback binds without TLS are
  rejected at startup.

## Web UI (later — designed for, not built)

Consumes gRPC (via grpc-web or a gateway). Planned views: provenance-graph
explorer (renders `Nodes` + `Chains` directly from EvidenceBundle), ask panel
(synthesized answers + impact timelines), sync dashboard (`Watch` stream). No
core changes required — this is why the bundle/synthesis split exists on the
gRPC surface.

## Configuration — `lore.yaml`

Two independent axes, deliberately separated
([00 — Δ9](00-design-deltas.md)): **sources** say what to *ingest*;
**repos** register *local clones* for code anchoring. Either may be empty
(but not both).

```yaml
workspace: myproject
index_path: ~/.lore/myproject.db           # default: ~/.lore/<workspace>.db

sources:                                    # ALL optional — configure what exists
  github:
    token_env: LORE_GITHUB_TOKEN            # env var NAME; value never stored
    repos:                                  # what to INGEST (no clone needed)
      - acme/myproject
      - acme/myproject-infra
  notion:
    token_env: LORE_NOTION_TOKEN
    root_pages:
      - "Engineering Wiki"                  # subtree scoping
  jira:
    base_url: https://acme.atlassian.net
    email_env: LORE_JIRA_EMAIL
    token_env: LORE_JIRA_TOKEN
    projects: [PROJ, INFRA]

repos: []                                   # OPTIONAL — local clones, blame/log only.
# repos:                                    # Zero repos = ask-only workspace;
#   - path: ~/dev/myproject                 # `why`/`history_of` disabled, all
#     remote: github:acme/myproject         # other tools fully functional.

query:                                      # optional tuning (server-capped)
  event_window: 30d                         # ± window for event resolution
  walk_depth: 3
  top_k: 12

embedder:
  provider: openai                          # openai | ollama
  model: text-embedding-3-small
  base_url: https://api.openai.com          # OPTIONAL — default: the provider's endpoint
# dimensions: 768                           # REQUIRED for ollama; `ollama show <model>` reports it

llm:                                        # OPTIONAL — synthesis for CLI/gRPC only
  provider: anthropic                       # openai | anthropic | zai | ollama
  model: claude-sonnet-4-5
  api_key_env: LORE_LLM_KEY
  base_url: https://api.anthropic.com       # OPTIONAL — default: the provider's endpoint

scheduler:
  interval: 30m

server:                                     # used by `lore serve`
  http_addr: ":8080"                        # MCP Streamable HTTP
  grpc_addr: ":9090"
  mtls:
    cert: ./certs/server.pem
    key: ./certs/server-key.pem
    client_ca: ./certs/ca.pem
```

Validation at load:

- Unknown keys rejected.
- At least one of `sources` / `repos` non-empty.
- `token_env`/`email_env` variables must exist when their source is configured.
- `repos[].remote` must name a configured source repo when enrichment mapping
  is intended; a clone without a matching source still blames, but chains stop
  at the commit layer (surfaced as a startup warning, not an error).
- Loopback/TLS rule enforced; embedder identity checked against the index
  `meta`.

At startup, when the embedder is constructed: `embedder.dimensions` is required
for the `ollama` provider — the width is configured, never probed, so the
identity is known without the daemon — and rejected for `openai`, where the
model implies it.

## Security posture

- Read-only against all external sources; least-privilege tokens documented
  (GitHub fine-grained PAT read scopes, Notion integration scoped to subtree,
  Jira API token with read-only project access).
- Secrets only via env vars; never written to `lore.yaml`, the index, or logs.
- Private data leaves the machine only toward the configured embedder/LLM —
  documented loudly; Ollama provider = fully local pipeline.
- gRPC/HTTP off-loopback requires TLS; gRPC additionally supports mTLS.
