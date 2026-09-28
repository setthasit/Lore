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
lore init                          # create workspace + lore.yaml scaffold + its JSON Schema
lore schema                        # rewrite the JSON Schema editors complete lore.yaml from
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

`lore-system-design.drawio` still shows the earlier secret contract, in which
a config key named an environment variable instead of holding the credential,
and needs a manual redraw on six pages: 1 ("env vars only"), 8 ("secrets only
via env vars" and its YAML example), 6 and 11 (a secret read "from the
variable the operator's `with:` entry names"), 9 ("env-only secrets") and 10
(frames at protocol version 1).

```yaml
workspace: myproject
index_path: ~/.lore/myproject.db           # default: ~/.lore/<workspace>.db

plugins:                                    # OPTIONAL — external plugins; see 10
  - name: linear
    from: github.com/jdoe/lore-linear@v0.3.1

sources:                                    # ALL optional — instances, in sync order
  - use: github                             # id defaults to the plugin name
    with:
      token: ${env:LORE_GITHUB_TOKEN}       # the credential itself, or ${env:VAR} to keep it out of the file
      repos:                                # what to INGEST (no clone needed)
        - acme/myproject
        - acme/myproject-infra
  - use: notion                             # no token: falls back to LORE_NOTION_TOKEN
    with:
      root_pages: ["Engineering Wiki"]      # subtree scoping
  - id: jira-acme                           # explicit id: two instances of one plugin
    use: jira
    with:
      base_url: https://acme.atlassian.net
      email: ${env:LORE_JIRA_EMAIL}
      token: ${env:LORE_JIRA_TOKEN}
      projects: [PROJ, INFRA]
  - id: jira-legacy
    use: jira
    with:
      base_url: https://legacy.atlassian.net
      email: ${env:LORE_JIRA_EMAIL}
      token: ${env:LORE_JIRA_LEGACY_TOKEN}
      projects: [OLD]
  - use: gitlab
    with:
      base_url: https://gitlab.com          # OPTIONAL — self-managed instances pass their root
      token: ${env:LORE_GITLAB_TOKEN}
      projects: [acme/myproject]            # merge requests map onto `pr`
  - id: linear
    use: linear                             # external plugin, identical syntax
    with: { team: PLATFORM, token: "${env:LORE_LINEAR_TOKEN}" }  # quoted: {} are flow syntax

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
      api_key: ${env:LORE_OPENROUTER_KEY}

embedder:                                   # role binding: provider instance + model
  provider: openai
  model: text-embedding-3-small
  api_key: ${env:OPENAI_API_KEY}            # OPTIONAL: only the provider's declared secrets, a literal like sk-live-abc works too
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

Loading is two-stage. First the skeleton above decodes strictly, except for
the keys a role binding carries beside `provider`, `model` and `dimensions`.
Then each `with:` block and each role binding's keys are validated against the
plugin's manifest, and a `with:` block is decoded strictly by the plugin itself.
Validation at load:

- Unknown keys rejected at four points: the top level by the schema
  (`internal/config/config.go`, `parse`, which sets `KnownFields(true)`),
  inside `with:` first against the manifest (`internal/registry/with.go`,
  `checkKeys`) and then by the plugin's own decoder (`sdk/host.go`,
  `SourceConfig.Decode`), and in a role binding against the provider's
  declared secrets (`internal/registry/with.go`, `checkRoleKeys`).
- Every `use:` resolves to a compiled plugin or a `plugins:` declaration, and
  an unresolved one names what this build has (`internal/registry/build.go`,
  `Registry.resolve` and `Registry.unresolved`).
- Duplicate instance ids rejected, so an id is required when one plugin is
  used twice (`internal/config/validate.go`, `validateInstances`).
- Required manifest fields present, and a required list field declared as an
  empty list refuses: an empty list selects nothing, so the instance would
  ingest nothing (`internal/registry/with.go`, `checkType`).
- A secret lives under its own key in `with:`, the manifest's `Secret.Key`
  (`token`, `email`, `api_key`), and that field holds the credential: a
  literal, or `${env:VAR}`. A field that is present must resolve to non-blank
  text, and a refusal names the field, plus the variable when it expanded one.
  When the field is absent, a plugin compiled into this binary falls back to
  the secret's `DefaultEnv`, and an unset or blank default stops the load,
  naming the field and that variable. An optional secret is skipped instead
  (`sdk/plugin.go`, `Secret`, and `internal/registry/secrets.go`,
  `resolveSecrets`).
- A plugin installed from outside the binary gets no fallback. Its declared
  default would steer the host onto a variable the operator never granted, so
  its `with:` block, or the role binding that names it, holds every required
  secret itself
  (`internal/registry/secrets.go`, `resolveSecrets`).
- A secret never reaches the plugin's configuration JSON, which is built from
  the declared fields alone; its value travels only in the secrets map
  (`internal/registry/with.go`, `configJSON`).
- A role binding carries its provider's secrets and nothing else. Beside
  `provider`, `model` and `dimensions`, `embedder:` and `llm:` accept each
  secret key the provider's manifest declares, written as in a `with:` block:
  a literal such as `api_key: sk-live-abc`, or `${env:VAR}`. The secret rules
  above apply to it, and a refusal names it as `embedder.api_key` or
  `llm.api_key`. Every other
  plugin setting, `base_url` or `preset` among them, belongs in the `with:`
  block of a `providers:` entry that the binding names. A binding that names a
  declared `providers:` entry carries no keys of its own
  (`internal/registry/with.go`, `checkRoleKeys`, and
  `internal/registry/build.go`, `checkCarriesNothing`).
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

### Environment expansion

A string value may take text from the environment. `${env:VAR}` is the only
accepted form, where VAR is made of upper-case letters, digits and
underscores and does not start with a digit, and one value may hold several:
`http_addr: ${env:LORE_HOST}:${env:LORE_HTTP_PORT}`. `$${` writes a literal
`${` and reads no variable; `$$` is an escape only directly before `{`, so
`pa$$word` stays as typed (`internal/envx`, `Expand`).

- A variable that is set but empty expands to empty text. An unset one stops
  the load, naming the field and the variable. Any other `${`, `${VAR}` and
  `${env:VAR:-default}` included, is refused, naming the field and the
  accepted form. No refusal quotes the field's text beyond the variable name,
  or any expanded value (`internal/envx`, `expandOne`).
- Every string field the skeleton decodes is expanded, and a refusal names it
  with its index, as `repos[2].path` (`internal/config/expand.go`,
  `Config.stringFields` and `instanceFields`). Durations and whole numbers
  parse during decoding and are not expanded.
- A `with:` block stays raw through the load, and the registry expands it
  when it prepares the instance (`internal/registry/expand.go`,
  `expandWith`). For a plugin compiled into this binary every string expands,
  inside lists and maps too. A plugin installed from outside the binary
  expands in its declared secret fields, whose values the host resolves and
  injects anyway. It also expands in each field its manifest marks
  `expandable`, including every string beneath that key. An expansion in any
  other declared field of such a plugin is refused, naming the field and the
  plugin, so the environment reaches a third party's settings only where its
  manifest asked for it. An expansion in a key the manifest does not declare
  is refused as an unknown key, as a literal there would be. Only a secret's
  value is scrubbed from output (see below), never an expandable field's, so a
  credential belongs in a declared secret.
- Because `plugins[].name` and every `use:` expand, the environment may
  choose which plugin a declaration runs. That is by design; the expanded
  name still resolves as any `use:` does (`internal/registry/build.go`,
  `Registry.resolve`).

`Config.expand` applies the `internal/envx` scanner in `Load` after strict
decoding and before defaults, so expansion runs before `~` expansion:
`index_path: ${env:LORE_INDEX}` with `LORE_INDEX=~/db/x.db` still resolves
under `$HOME`. It walks named struct fields because `yaml.v3`'s `Node.Decode`
ignores `KnownFields`.

`config.ReadFile` and `config.Decode` never expand, so the commands that edit
`lore.yaml` as text splice the file as written. The `lore plugin` commands
expand, through `Config.ExpandPluginRefs` into a `config.PluginRefs` copy,
only `plugins[]` and the `id`, `use` and `path` fields that name a plugin or
its users, so an unset variable in `workspace` does not stop them. They never
write an expanded value back: an update, or an install that resolves
`@latest`, that would pin a `from:` written with `${env:VAR}` is refused,
naming the field, and `lore plugin list` shows such a `from` by its field
name (`internal/plugindist/workspace.go`, `Workspace.onDisk` and
`Workspace.DeclaredFrom`).

### Editor support

`lore schema` writes a JSON Schema (draft-07) beside the configuration, named
after it: `lore.yaml` gets `lore.schema.json`. `lore init` writes it once and
opens the scaffold with the modeline
`# yaml-language-server: $schema=./lore.schema.json`, which any editor running
yaml-language-server (VS Code's YAML extension, Neovim's `yamlls`, Helix)
reads for key and value completion, hover docs and diagnostics
(`internal/configschema`, `Generate`, called from
`internal/transport/cli/schema.go`, `runSchema`).

The schema is generated from the skeleton above and from each plugin's
manifest. Compiled-in plugins come from the registry. A plugin declared under
`plugins:` comes from the manifest its install recorded, read the same way
`lore source add` reads it. It mirrors the loader: unknown keys are flagged at
the top level, in `with:` and in a role binding. `use:` offers the plugins of
the matching kind, a `with:` block offers and types the keys its plugin
declares, and `embedder:`/`llm:` offer the secrets of the provider they name.
A secret is required only where nothing falls back, so an external plugin's
secret is always required (`registry.FallsBack`). A declared plugin whose
manifest cannot be read is still accepted by `use:`, its `with:` goes
unchecked, and the command prints the reason as an `unchecked:` line.

It never replaces the loader. It cannot see the environment, whether a
`providers[]` id exists, or a plugin's own decoding rules. Durations are
matched against the `lore.ParseDuration` grammar, and `${env:VAR}` is accepted
wherever the loader would expand it. Regenerate it after installing, updating
or removing a plugin.

## Security posture

- Read-only against all external sources by contract, not by enforcement.
  `lore.Connector` and `lore.CodeRepo` expose no write method, so nothing in
  the engine can ask a source or a clone to change, and what a plugin's own
  client does is the plugin's own business
  ([04](04-connectors-and-sync.md#connector-contract)). Least-privilege tokens
  are the control that holds: GitHub fine-grained PAT read scopes, GitLab
  token with `read_api`, Notion integration scoped to subtree, Jira API token
  with read-only project access.
- A secret field holds its credential. Written as `${env:VAR}`, the value
  stays out of `lore.yaml`. A literal is allowed, and startup announces it
  once on stderr, naming the fields and never the value: `lore: secrets
  written as literal values in the config: sources[github].with.token; write
  ${env:VAR} to keep a credential out of the file` (`internal/secrets`,
  `Sink.Notices`, printed by `internal/transport/cli/runtime.go`,
  `noticeLedger.print`). No credential is written to the index. Every
  resolved value, literal, expanded or read from a default variable, is
  scrubbed from output as below, and a refusal names the field, and the
  variable when there is one, never the value
  (`internal/registry/secrets.go`, `resolveSecrets`). An external plugin's own
  diagnostics are redacted as they cross the pipe (`sdk/stdio`, `redacting`),
  and a plugin compiled into this binary logs through the host's diagnostic
  logger (`internal/registry/build.go`, `Registry.Host`), which the host
  scrubs as below.
- The host resolves each secret and injects the value, so a plugin
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
- The scrub misses a value shorter than 8 characters; its field, or for a
  default its variable and instance, is named in one stderr line at startup
  instead (`lore: secrets shorter than 8 characters are not scrubbed: ...`).
  An encoding nested deeper than three levels passes. Each write is scrubbed
  on its own, so a value split across two writes passes intact
  (`internal/secrets/writer.go`). A multi-line value
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
