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
lore source add <plugin>           # append source config interactively (prompts from the manifest)
lore plugin list                   # every plugin this build can use, with kind and origin
lore plugin install <name|coord>   # fetch, verify and install an external plugin
lore plugin update|remove <name>   # re-resolve a coordinate / delete an install
lore plugin verify <name>          # run the conformance suite against an installed plugin
lore plugin search <query>         # query the plugin index
lore build --with <coordinate>     # build a custom lore binary with a plugin compiled in
lore sync [--source=jira-acme] [--reembed]
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

Three independent axes: **plugins** declare what code may run, **sources**
declare instances to *ingest*, and **repos** register *local clones* for code
anchoring. Sources and repos may each be empty (but not both). Every `use:`
names a plugin — official, third-party-compiled, or external
([08](08-extensibility.md)).

```yaml
workspace: myproject
index_path: ~/.lore/myproject.db           # default: ~/.lore/<workspace>.db

plugins:                                    # OPTIONAL — external plugins; see 10
  - name: linear
    from: github.com/jdoe/lore-linear@v0.3.1

sources:                                    # ALL optional — instances, in sync order
  - use: github                             # id defaults to the plugin name
    with:
      token_env: LORE_GITHUB_TOKEN          # env var NAME; value never stored
      repos:                                # what to INGEST (no clone needed)
        - acme/myproject
        - acme/myproject-infra
  - use: notion
    with:
      token_env: LORE_NOTION_TOKEN
      root_pages: ["Engineering Wiki"]      # subtree scoping
  - id: jira-acme                           # explicit id: two instances of one plugin
    use: jira
    with:
      base_url: https://acme.atlassian.net
      email_env: LORE_JIRA_EMAIL
      token_env: LORE_JIRA_TOKEN
      projects: [PROJ, INFRA]
  - id: jira-legacy
    use: jira
    with:
      base_url: https://legacy.atlassian.net
      email_env: LORE_JIRA_EMAIL
      token_env: LORE_JIRA_LEGACY_TOKEN
      projects: [OLD]
  - use: gitlab
    with:
      base_url: https://gitlab.com          # OPTIONAL — self-managed instances pass their root
      token_env: LORE_GITLAB_TOKEN
      projects: [acme/myproject]            # merge requests map onto `pr`
  - id: linear
    use: linear                             # external plugin, identical syntax
    with: { team: PLATFORM, token_env: LORE_LINEAR_TOKEN }

repos: []                                   # OPTIONAL — local clones, blame/log only.
# repos:                                    # Zero repos = ask-only workspace;
#   - path: ~/dev/myproject                 # `why`/`history_of` disabled, all
#     use: git                              # other tools fully functional.
#     remote: github:acme/myproject         # `use` defaults to the git plugin.

query:                                      # optional tuning (server-capped)
  event_window: 30d                         # ± window for event resolution
  walk_depth: 3
  top_k: 12

providers:                                  # OPTIONAL — a provider id that names a
  - id: openrouter                          # registered plugin and needs no options
    use: openai-compatible                  # may be referenced without declaring it
    with:
      base_url: https://openrouter.ai/api
      api_key_env: LORE_OPENROUTER_KEY

embedder:                                   # role binding: provider instance + model
  provider: openai
  model: text-embedding-3-small
# dimensions: 768                           # REQUIRED for ollama; `ollama show <model>` reports it

llm:                                        # OPTIONAL — synthesis for CLI/gRPC only
  provider: openrouter
  model: moonshotai/kimi-k2

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

Loading is two-stage: the skeleton above decodes strictly, then each `with:`
block is validated against its plugin's manifest and decoded strictly by the
plugin itself. Validation at load:

- Unknown keys rejected — at the top level by the schema, inside `with:` by the
  plugin's own decoder.
- Every `use:` resolves to a compiled plugin or a `plugins:` declaration; an
  unresolved one names what this build has.
- Duplicate instance ids rejected; an id is required when one plugin is used
  twice.
- Required manifest fields present; `*_env` variables exist when their instance
  is configured.
- At least one of `sources` / `repos` non-empty.
- `embedder.provider` and `llm.provider` resolve to provider instances whose
  plugins declare the matching capability.
- `repos[].remote` must name a configured source instance when enrichment
  mapping is intended; a clone without a matching source still blames, but
  chains stop at the commit layer (a startup warning, not an error). The
  warning applies to any source plugin declaring `RepoRemotes`.
- Loopback/TLS rule enforced; embedder identity checked against the index
  `meta`.
- Declared external plugins are installed and digest-matched; nothing is
  fetched at load or sync time ([10](10-plugin-distribution.md)).

At startup, when the embedder is constructed: `embedder.dimensions` is required
for the `ollama` provider — the width is configured, never probed, so the
identity is known without the daemon — and rejected for `openai`, where the
model implies it. Which of the two applies is the driver's rule, not the
engine's.

## Security posture

- Read-only against all external sources; least-privilege tokens documented
  (GitHub fine-grained PAT read scopes, GitLab token with `read_api`, Notion
  integration scoped to subtree, Jira API token with read-only project access).
- Secrets only via env vars; never written to `lore.yaml`, the index, or logs.
  Config names variables; the host resolves and injects them, and a plugin sees
  only the secrets its manifest declared — never the ambient environment.
- Private data leaves the machine only toward the configured embedder/LLM —
  documented loudly; Ollama provider = fully local pipeline.
- gRPC/HTTP off-loopback requires TLS; gRPC additionally supports mTLS.
- An external plugin executes with the user's privileges: installation is
  explicit, digests are pinned in `lore.lock`, and a mismatch refuses to launch
  ([10](10-plugin-distribution.md#trust-model)).
