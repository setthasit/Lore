# 06 — Interfaces & Config

## MCP (stdio + Streamable HTTP)

Implemented with the official Go MCP SDK
([modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk)).
Every query tool returns structured `EvidenceBundle` JSON, never synthesized
prose ([02 — D1](02-architecture.md#key-design-decisions)). The two sync tools
answer their own shapes: `sync_now` a round acknowledgment, `sync_status` an
index status (`internal/transport/mcp/sync.go`, `syncAcknowledgment` and
`indexStatus`). Every tool but `sync_now` carries `readOnlyHint` on its
registration: `registerSyncStatus` sets it and `registerSyncNow` does not
(`internal/transport/mcp/sync.go`), and each query tool sets it in its own
file. Only `sync_now` mutates local state, which is the index and never a
source.

| Tool | Input | Returns |
|---|---|---|
| `find_decision` | `question`, optional `around` (event text or date), `source`, `repo`, `doc_type`, `since`, `until` | EvidenceBundle seeded by retrieval; works with zero repos |
| `why` | `file`, `line_start`, optional `repo`, `line_end`, `question` | EvidenceBundle anchored on blame, precondition error if no repos registered. Omitting `line_end` blames `line_start` alone, and omitting `repo` works when one clone is registered |
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
The deprecation convention lives there too, and it is a rule for changes to
this repository rather than anything the code checks. No tool is deprecated
today and no registration carries such a description.

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
lore ask "<question>" [--around="incident X"] [--since --until] [--source --repo --doc-type]   # → find_decision
lore impact <ref | "query"> [--question="…"]
lore why <file>:<L1>-<L2> ["question"] [--repo=…]
lore trace <ref> [--direction=in|out|both]
lore history <path> [--repo=…] [--limit=N] [--before=<sha>]
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

- Every query request has `optional bool synthesize`, and an absent value reads
  as true on this surface (`internal/transport/grpc/query.go`, `synthesize`).
  Responses carry the raw `EvidenceBundle` **and** optional `synthesis` text —
  the web UI renders the provenance graph from the bundle and prose from the
  synthesis.
- `SyncService.Watch` streams sync progress — needed for a UI progress view.

### mTLS

- Mutual TLS on the gRPC listener, with **required client-certificate
  verification** against a configured client CA
  (`tls.RequireAndVerifyClientCert`). Two things turn it on: `lore serve
  --mtls`, or `server.mtls.client_ca` alone in `lore.yaml`. Either way all three
  of `cert`, `key` and `client_ca` must be set, and the two refusals differ.
  `serverTLS` runs first and refuses exactly one case as a bad request, a
  `cert` without its `key` or a `key` without its `cert`. Every other
  incomplete shape reaches `grpcTransportTLS`, which refuses it as a
  precondition error naming every setting still missing
  (`internal/transport/cli/serve.go`, `serverTLS` and `grpcTransportTLS`).
- Config: server cert/key + client CA bundle paths in `lore.yaml`; a
  `make certs.dev` target generates a local CA + server/client certs for
  development.
- Neither listener may bind a non-loopback address in the clear. An address that
  is not provably a loopback IP is refused at startup unless `server.mtls.cert`
  and `server.mtls.key` are both set (`internal/config/validate.go`,
  `Config.ValidateListenAddr`).

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

query:                                      # optional tuning, operator values used as given
  event_window: 30d                         # ± window for event resolution
  walk_depth: 3                             # default when unset or zero, negative refused at load, no upper bound
  top_k: 12                                 # default when unset or zero, negative refused at load, no upper bound

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

- Unknown keys rejected at three points: the top level by the schema
  (`internal/config/config.go`, `parse`, which sets `KnownFields(true)`),
  inside `with:` first against the manifest (`internal/registry/with.go`,
  `checkKeys`) and then by the plugin's own decoder (`sdk/host.go`,
  `SourceConfig.Decode`).
- Every `use:` resolves to a compiled plugin or a `plugins:` declaration, and
  an unresolved one names what this build has (`internal/registry/build.go`,
  `Registry.resolve` and `Registry.unresolved`).
- Duplicate instance ids rejected, so an id is required when one plugin is
  used twice (`internal/config/validate.go`, `validateInstances`).
- Required manifest fields present, and a required list field declared as an
  empty list refuses: an empty list selects nothing, so the instance would
  ingest nothing (`internal/registry/with.go`, `checkType`).
- A secret the manifest does not mark optional needs a named variable that
  holds a value. An optional secret is skipped when nothing names its variable,
  and, for a plugin compiled into this binary, when the only name came from the
  plugin's default and that variable is unset. Once a `with:` entry names the
  variable, that variable must be set
  (`sdk/plugin.go`, `Secret.Optional`, and `internal/registry/secrets.go`,
  `resolveSecrets`).
- A plugin's declared default variable applies only to a plugin compiled into
  this binary. For a plugin installed from outside the binary the declared
  default is never used, because it would steer the host onto a variable the
  operator never granted, so a required secret needs that plugin's `with:`
  block to name the variable itself (`internal/registry/secrets.go`,
  `resolveSecrets`).
- At least one of `sources` / `repos` non-empty
  (`internal/config/validate.go`, `Config.Validate`).
- `embedder.provider` and `llm.provider` resolve to provider instances whose
  plugins declare the matching capability (`internal/registry/build.go`,
  `Registry.BuildProvider`).
- `repos[].remote` must name a configured source instance when enrichment
  mapping is intended; a clone without a matching source still blames, but
  chains stop at the commit layer (a startup warning, not an error). The
  warning applies to any source plugin declaring `RepoRemotes`.
- The loopback/TLS rule is checked by `lore serve` alone. `Config.Validate`
  (`internal/config/validate.go`) never reads a listen address, so a
  `lore.yaml` naming `0.0.0.0:8080` loads without complaint for `lore sync`,
  `lore ask` or `lore mcp`. `lore serve` refuses it before either listener
  binds (`internal/transport/cli/serve.go`, `serve`, through
  `Config.ValidateListenAddr`).
- The embedder identity is not compared here: the index's recorded identity is
  read by a sync round and by `lore --version`, never at load
  ([03](03-data-model.md)).
- Declared external plugins are resolved and digest-matched when the workspace
  is constructed (`internal/di/external.go`, `newExternals`), which every
  command that opens a workspace reaches, before the scheduler starts and
  before any source is touched. Nothing is fetched there, and nothing is
  fetched in a sync round ([10](10-plugin-distribution.md)).

At startup, when the embedder is constructed: `embedder.dimensions` is required
for the `ollama` provider — the width is configured, never probed, so the
identity is known without the daemon — and rejected for `openai`, where the
model implies it. Which of the two applies is the driver's rule, not the
engine's.

## Security posture

- Read-only against all external sources by contract, not by enforcement.
  `lore.Connector` and `lore.CodeRepo` expose no write method, so nothing in
  the engine can ask a source or a clone to change, and what a plugin's own
  client does is the plugin's own business
  ([04](04-connectors-and-sync.md#connector-contract)). Least-privilege tokens
  are the control that holds: GitHub fine-grained PAT read scopes, GitLab
  token with `read_api`, Notion integration scoped to subtree, Jira API token
  with read-only project access.
- Secrets only via env vars, never written to `lore.yaml` or the index. Config
  names the variables and nothing else carries a credential, and a refusal
  names the variable rather than the value behind it
  (`internal/registry/secrets.go`, `resolveSecrets`). An external plugin's own
  diagnostics are redacted as they cross the pipe (`sdk/stdio`, `redacting`),
  and a plugin compiled into this binary logs through the host's diagnostic
  logger (`internal/registry/build.go`, `Registry.Host`), which the host
  scrubs as below.
- The host resolves each named variable and injects the value, so a plugin
  receives only the secrets its manifest declared. For an external plugin the
  host also withholds its own environment, handing the subprocess an empty one
  and, on Windows, the variables the loader and runtime need
  (`internal/plugexec/env.go`, `minimalEnv`). A plugin compiled into this
  binary shares the host process, so there the rule is a contract it keeps
  rather than a boundary the host draws.
- The host scrubs resolved secret values from what it emits
  (`internal/secrets`). Resolving a secret records the value with one `Sink`
  in every escaped form an output produces: `strconv` quoting and JSON with
  and without HTML escaping, in any mix nested up to three levels, and the
  CLI's control-character escaping (`Sink.Record`). `app.Run` builds that sink
  and the one diagnostic logger, which writes through it, so a compiled-in
  plugin's log, an external plugin's relayed stderr, and the scheduler, gRPC
  and MCP diagnostics are all scrubbed. A lint rule (`.golangci.yml`,
  `forbidigo`) rejects any other host text or JSON handler and the default and
  standard loggers. Relayed stderr is logged at Debug and that logger prints
  from Info, so it never prints (`internal/plugexec/session.go`, `stderrLog`).
  The CLI's own stdout and stderr, the `lore mcp` stream included, pass the
  sink (`internal/transport/cli/root.go`, `execute`), as do `lore serve`'s
  MCP-over-HTTP response bodies (`internal/transport/mcp/http.go`,
  `scrubResponses`) and its gRPC response strings and status messages
  (`internal/transport/grpc/scrub.go`).
- The scrub misses a value shorter than 8 characters; its variable and field
  are named in one stderr line at startup instead (`lore: secrets shorter than
  8 characters are not scrubbed: ...`). An encoding nested deeper than three
  levels passes. Each write is scrubbed on its own, so a value split across two
  writes passes intact (`internal/secrets/writer.go`). A multi-line value
  inside an excerpt the CLI indents passes too, since the indent breaks the
  recorded form. HTTP response headers and gRPC metadata are not scrubbed;
  none carries plugin text today.
- Private data leaves the machine only toward the configured embedder/LLM —
  documented loudly; Ollama provider = fully local pipeline.
- gRPC/HTTP off-loopback requires TLS; gRPC additionally supports mTLS.
- An external plugin executes with the user's privileges: installation is
  explicit, the digest recorded by the first install of a remote coordinate is
  enforced from then on at every launch and on every later install that is not
  an update, and a mismatch refuses to launch
  ([10](10-plugin-distribution.md#trust-model)). An unsigned
  `https://` coordinate is the one shape nothing constrains, on its first
  install and again on every `lore plugin update`, because `Install` compares a
  digest only when `hasLocked && !req.Rewrite`
  (`internal/plugindist/install.go`, `Install`, and
  [10](10-plugin-distribution.md#what-each-install-shape-verifies)).
