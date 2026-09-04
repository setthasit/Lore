# 09 — Plugin Protocol (external plugins)

This document specifies the wire protocol between the host and an
**out-of-process** plugin; compiled plugins implement the Go interfaces in
[08](08-extensibility.md) and encode nothing. Both modes present the identical
`lore.Connector` / provider surface to the engine, so the engine never learns
which mode a plugin uses. Where a binary comes from — coordinates, resolution,
install layout, signatures — is [10](10-plugin-distribution.md), not here.

## Status

**The protocol is not frozen.** It freezes when the milestone that ships
external plugins (M10) has landed *and* the plugin contract has stopped
moving; until then it may change in any direction, including non-additively.
Δ10 in [00](00-design-deltas.md) is why: a wire signature frozen before its
contract settled turned an internal fix into an unimplementable contract, and
a published protocol turns that class of fix into an ecosystem break.

## Transport

The host spawns the plugin binary as a child process: requests go to its
**stdin**, responses come back on its **stdout**, one JSON object per line
(NDJSON), UTF-8, `\n`-terminated, with no raw newline inside an object.

- **stdout is protocol-only.** A plugin that prints a banner, a progress bar,
  or a stray `fmt.Println` there is malformed and fails — no resynchronization.
- **stderr is free-form** and forwarded to the host logger at debug level,
  prefixed with the instance id; it is the only diagnostic channel.
- **Line size cap: 8 MiB.** A longer line fails the operation, and a batch too
  large to frame MUST be split into several `batch` frames — batches are the
  checkpoint unit, so splitting is always legal.
- **Backpressure is the OS pipe.** The host reads the next frame only after
  committing the previous batch, so a fast plugin blocks on `write` instead of
  buffering a whole source; a plugin MUST NOT defeat this by buffering itself.

Examples below are wire-form — one object per line — except the two
multi-line objects, which are pretty-printed for readability.

## Envelope

```json
{ "v": 1, "id": "7f3a", "op": "changes" }
{ "v": 1, "id": "7f3a", "ok": true }
{ "v": 1, "id": "7f3a", "error": { "message": "…", "retryable": false, "kind": "auth" } }
```

- `v` is the protocol version, equal to `lore.APIVersion` (1); a plugin
  receiving a `v` it does not implement MUST answer with an error naming both
  numbers, never guess.
- `id` is host-generated and opaque; the plugin echoes it **verbatim** on every
  frame it emits for that request, streamed frames included.
- **One in-flight request per process** — a deliberate simplification, because
  concurrency is the host's job and it gets it by running more processes, so a
  plugin needs no scheduler. The next request follows `ok`, `error`, or `done`.
- Unknown response fields are ignored by the host, unknown request fields MUST
  be ignored by the plugin; evolution is additive only, which is what makes
  ignoring safe both ways.

## Operations

| op | Kind | Direction | Payload | Response |
|---|---|---|---|---|
| `manifest` | all | request/response | — | `manifest` |
| `changes` | `KindSource` | request/**stream** | `instance`, `config`, `secrets`, `cursor` | `batch` frames, then `done` |
| `embed` | `KindProvider` (Embed) | request/response | `config`, `secrets`, `model`, `texts` | `vectors`, `dimensions` |
| `complete` | `KindProvider` (Complete) | request/response | `config`, `secrets`, `model`, `system`, `user` | `text` |
| `blame` | `KindCode` | request/response | `path`, `start_line`, `end_line` | `spans` |
| `log` | `KindCode` | request/response | `path` | `commits` |
| `shutdown` | all | request/exit | — | `ok`, then exit 0 |

`KindCode` payloads carry no `config` or `secrets`: `path` is
workspace-absolute — the host resolves it against the registered clone root
before sending — and a local clone needs no credentials.

### manifest

The first request on every process; the host validates config against `fields`
and resolves `secrets` before any other op.

```json
{ "v": 1, "id": "1", "op": "manifest" }
```

```json
{
  "v": 1, "id": "1", "ok": true,
  "manifest": {
    "name": "linear", "kind": "source", "api_version": 1,
    "summary": "Linear issues and comments; created_at is the issue createdAt",
    "capabilities": { "embed": false, "complete": false, "repo_remotes": false },
    "fields": [
      { "name": "teams", "type": "string_list", "required": true },
      { "name": "base_url", "type": "string", "required": false }
    ],
    "secrets": [{ "key": "api_key", "config_field": "token_env", "default_env": "LORE_LINEAR_TOKEN" }]
  }
}
```

The host MUST reject a manifest whose `api_version` differs from its own with a
message naming **both** numbers (`plugin "linear" speaks api_version 2, host
speaks 1`), never a generic mismatch error. `capabilities` is an object of
booleans, all false for `KindSource` and `KindCode` except `repo_remotes`; a
`KindProvider` sets `embed`, `complete`, or both, and the host refuses a role
binding to a capability the plugin did not declare.

### changes

Streams documents modified since `cursor`, oldest-first: zero or more `batch`
frames, then exactly one `done`. An empty `cursor` object means full backfill.

```json
{ "v": 1, "id": "2", "op": "changes", "instance": "linear-core", "config": { "teams": ["CORE", "PLAT"] }, "secrets": { "api_key": "lin_api_…" }, "cursor": { "updated_after": "2026-08-31T09:12:44Z", "last_id": "ENG-4471" } }
{ "v": 1, "id": "2", "batch": { "docs": [], "cursor": { "updated_after": "2026-09-01T00:00:00Z" } } }
{ "v": 1, "id": "2", "done": true }
```

Every `batch` frame MUST carry a `cursor`, including one whose `docs` is empty.
The host commits the documents, **then** persists that frame's cursor — the
batch is the checkpoint unit, exactly as for in-process connectors
([04](04-connectors-and-sync.md)). A stream that emits documents and defers its
cursor to `done` is malformed: it makes crash-safe resume unimplementable.

### embed

```json
{ "v": 1, "id": "3", "op": "embed", "config": { "base_url": "http://127.0.0.1:11434" }, "secrets": {}, "model": "nomic-embed-text", "texts": ["why option B over A", "rollback plan"] }
{ "v": 1, "id": "3", "ok": true, "vectors": [[0.0131, -0.0442], [-0.0087, 0.0210]], "dimensions": 768 }
```

`vectors` MUST be positionally aligned with `texts` and of equal length; a
short, reordered, or filtered result is a protocol error, never a partial
success. The plugin reports `dimensions`, never an identity string — the host
composes the vector-space identity as `<plugin>/<model>/<dims>`
([08](08-extensibility.md)), so no plugin can claim another's identity.

### complete

```json
{ "v": 1, "id": "4", "op": "complete", "config": {}, "secrets": { "api_key": "sk-…" }, "model": "claude-sonnet-4", "system": "Answer only from the evidence.", "user": "Why did we pick B over A?" }
{ "v": 1, "id": "4", "ok": true, "text": "B was chosen because …" }
```

Empty or whitespace-only `text` is an **error**, not a success: an empty
completion is indistinguishable from a dropped request, reported as `internal`.

### blame and log

```json
{ "v": 1, "id": "5", "op": "blame", "path": "/w/api/internal/auth/auth.go", "start_line": 40, "end_line": 42 }
{ "v": 1, "id": "5", "ok": true, "spans": [{ "sha": "9c1f0ab3e5d4", "line_start": 40, "line_end": 42, "author": "Ada Lovelace", "time": "2026-05-14T08:31:02Z", "lines": ["if !tok.Valid() {", "\treturn errUnauthorized", "}"] }] }
{ "v": 1, "id": "6", "op": "log", "path": "/w/api/internal/auth/auth.go" }
{ "v": 1, "id": "6", "ok": true, "commits": [{ "sha": "9c1f0ab3e5d4", "author": "Ada Lovelace", "time": "2026-05-14T08:31:02Z", "subject": "reject expired tokens" }] }
```

Spans come in span order, `lines` holds one entry per line in the span, and
`commits` is newest-first following renames. Both ops are read-only — a code
plugin never writes to the clone.

### shutdown

```json
{ "v": 1, "id": "7", "op": "shutdown" }
{ "v": 1, "id": "7", "ok": true }
```

The plugin answers, flushes stdout, and exits `0`; it MUST NOT start new work.

## Data encoding

Field names are snake_case, one-to-one with the entity fields: `Document` →
`id`, `source`, `type`, `repo_ref`, `title`, `body`, `author`, `url`,
`created_at`, `updated_at`, `refs`; `RawRef` → `kind`, `value`; `Batch` →
`docs`, `cursor`.

```json
{
  "batch": {
    "docs": [{
      "id": "linear:ticket:ENG-4471", "source": "linear", "type": "ticket",
      "repo_ref": "",
      "title": "Move session store to Redis",
      "body": "Chose B (Redis) over A (sticky sessions) because …",
      "author": "grace@example.com",
      "url": "https://linear.app/acme/issue/ENG-4471",
      "created_at": "2026-08-30T14:02:11Z",
      "updated_at": "2026-09-01T07:45:03+02:00",
      "refs": [
        { "kind": "url", "value": "https://github.com/acme/api/pull/812" },
        { "kind": "ticket_key", "value": "PROJ-123" },
        { "kind": "commit_sha", "value": "9c1f0ab" }
      ]
    }],
    "cursor": { "updated_after": "2026-09-01T05:45:03Z", "last_id": "ENG-4471" }
  }
}
```

- `id` is `"<source>:<type>:<external_id>"` and the host never rebuilds it;
  `source` MUST equal the request's `instance` and `id` MUST carry it as its
  prefix, or the host fails the batch.
- `repo_ref` is `"github:owner/repo"` for repository-scoped documents, empty
  otherwise; the key is always present, the value may be empty.
- Timestamps are RFC 3339 **with** a timezone offset. `created_at` and
  `updated_at` are both REQUIRED and non-zero; a source with no true creation
  time sets `created_at = updated_at` and says so in its manifest `summary`,
  the rule in-process connectors follow in their package doc.
- `refs[].kind` MUST be one of `url`, `ticket_key`, `commit_sha`, `file_path`,
  `pr_number`; an unknown kind is rejected at ingest with the known list, never
  dropped — a dropped ref is a missing edge and so a wrong answer.
- `type` is an open set: an unknown value is accepted, chunked with the default
  strategy, and ranked as ordinary evidence.
- `cursor` is a flat string→string map, opaque to the host, which stores and
  replays it without inspecting a key.

## Errors

```json
{ "v": 1, "id": "2", "error": { "message": "token lacks read:issues", "retryable": false, "kind": "auth" } }
```

| `kind` | Host action |
|---|---|
| `invalid_config` | fail the round immediately; no retry, no backoff |
| `auth` | fail the round immediately; credentials do not fix themselves |
| `rate_limit` | back off, resume from the last committed cursor |
| `not_found` | fail this connector; the round continues with the others |
| `internal` | fail this connector; the round continues with the others |

Any error with `retryable: true` is backed off and resumed from the last
committed cursor whatever its `kind`; `rate_limit` implies it, an unknown
`kind` is treated as `internal`, and `retryable` is authoritative for
scheduling. A plugin MUST NOT exit non-zero as its way of reporting a business
error: a non-zero exit means *the process died*, and the host reports it as a
crash naming the instance and the last op. Every expected failure — bad
credentials, missing resource, throttling — is an `error` frame on stdout,
after which the process stays alive and ready for the next request.

## Lifecycle and cancellation

One process per instance per round: the host spawns, sends `manifest`, runs
the operation (a whole `changes` stream for a source, one call for a provider
or code op), then `shutdown`; no state survives a round except what the plugin
put in the cursor. Cancellation escalates in three steps — close stdin, then
`SIGTERM`, then `SIGKILL` after a 5s grace — and a plugin MUST treat stdin EOF
as cancel, abandoning in-flight work and exiting. The host MUST NOT assume a
killed plugin flushed anything, which is safe because the last persisted
cursor is authoritative: unflushed frames are work the next round redoes, and
re-ingest is idempotent by `id`.

| Operation | Timeout |
|---|---|
| `manifest` | 10s |
| `embed`, `blame`, `log` | 60s |
| `complete` | 120s, matching the in-process LLM `RequestTimeout` |
| `changes` | none while frames keep arriving; 300s idle |
| `shutdown` | 5s, then the escalation above |

The `changes` timeout is idle-only — a long backfill is legitimate, a silent
process is not.

## Secrets

Secrets travel **inside the request payload on stdin**: never argv, which is
world-readable in `ps`, and never the inherited environment, so a plugin sees
only what its manifest declared. The host resolves the env var names given in
`lore.yaml` and delivers values keyed by the manifest's secret keys
(`{"api_key": "…"}`), so a plugin's key names are its own, independent of the
operator's variable naming. A plugin MUST NOT read `os.Getenv` or any
equivalent; needing an undeclared value means it is misconfigured.

The transport changes no residual risk: a source plugin holds a source token,
so "read-only" is a promise the host cannot enforce for a subprocess — trust
controls are in [10](10-plugin-distribution.md).

## Conformance

`sdk/conform` runs against an external plugin **unchanged**, because it needs
only a `lore.Connector` and the client shim that speaks this protocol is one —
so one suite certifies compiled and external plugins identically. It asserts:

- resumability from a mid-stream cursor: replaying from batch *n*'s cursor
  yields batch *n+1* onward, no gap and no rewind;
- idempotency: a full replay produces no duplicate documents, since upserts
  key on `id`;
- every batch carries a cursor, empty ones included;
- `created_at` and `updated_at` both set and non-zero on every document;
- full identity on every document — `id`, `source`, `type`, `url` present and
  `id` consistent with its three parts.

`lore plugin verify` is this suite pointed at an installed binary — the same
code path a third-party author runs locally before publishing.

## Non-goals

- **No host callbacks, no bidirectional RPC.** A plugin cannot ask the host
  anything; one that wants the store is misdesigned and violates the layering
  in [02](02-architecture.md) — connectors produce data, services orchestrate.
- **No plugin-to-plugin communication.** Plugins do not know about each other;
  cross-source relations are the LinkResolver's job.
- **No long-lived daemon plugins.** A process exists for a round; anything
  needing persistence goes in the cursor.
- **No gRPC.** Rejected because it forces a proto toolchain and generated code
  on every plugin author, the cost NDJSON avoids; it stays the escape hatch if
  host callbacks ever become necessary, when hand-rolling stops being cheap.
- **Not Go's `plugin` package.** Version-locked to the exact toolchain and
  dependency graph, no Windows, and it breaks the pure-Go build.

Sync is I/O bound, so a subprocess boundary costs nothing measurable against
network round-trips — which is why exec is the default for third-party plugins
rather than a fallback.

## Reference implementation

`sdk` ships the Go side of this protocol: an author writes a `lore.Connector`
and a `main` handing it to the SDK's serve helper — no framing, no JSON, no
signal handling. Nothing here requires the SDK: the protocol is small enough
to implement in Python or TypeScript by reading lines from stdin and printing
objects to stdout, and that portability is why NDJSON was chosen.
