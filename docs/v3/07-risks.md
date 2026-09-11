# 07 — Risks & Open Questions

The system is built; this document is what stays true afterwards. The risks
below describe operating conditions, not outstanding tasks — each one names the
mitigation that is in the code today, so a change that removes the mitigation
has to argue with a row here first.

## Named risks

| Risk | Impact | Mitigation |
|---|---|---|
| GitHub rate limits on large-repo backfill | first sync slow/fails | GraphQL batching, batch-level cursor checkpoints (resumable), backoff on secondary limits |
| Jira/Notion API slow pagination | long backfill | project/subtree scoping; backfill is async and batch-resumable by design |
| Jira API churn (legacy search endpoints deprecated) | connector breakage | built on `/search/jql` + `nextPageToken`; the connector is isolated in one plugin package |
| Event resolution picks the wrong "incident X" | wrong time window → misleading evidence | agreement check across top hits; ambiguity → explicit Gap with candidates instead of a silent guess; resolved anchor always returned in `Anchor.Window` for auditability |
| Timestamp skew (docs edited long after the event) | wrong timeline ordering | `CreatedAt` (event time) drives windows/timelines; `UpdatedAt` only drives sync/freshness |
| Sparse linking (no PR discipline, tickets never referenced) | thin chains, weak answers | `Gaps` reporting keeps answers honest; retrieval seeding still finds unlinked discussions; docs set expectations |
| Private data → cloud embedder/LLM | privacy concern | pluggable providers; Ollama = fully local; documented loudly |
| WASM SQLite slower than cgo | query latency | store benchmarks (`internal/repositories/sqlite/bench_test.go`) run against a realistic corpus; a cgo pairing is a drop-in behind `IndexStore` if the numbers ever demand it |
| Embedding model change invalidates vectors | silent quality loss | embedder identity in `meta`; startup mismatch check; explicit `--reembed` |
| Third-party plugin holds a source token | a compromised or malicious plugin exfiltrates data | per-plugin secret injection (an out-of-process plugin starts with an empty environment and receives only what its manifest declared), mandatory digest pinning, explicit installation, optional signatures, `lore plugin verify`; a WASM sandbox is the named enforcement tier for untrusted authors |
| Plugin contract churn now that third parties exist | ecosystem breakage | `APIVersion` checked at registration and in the protocol handshake; the wire protocol ([09](09-plugin-protocol.md)) is frozen and evolves additively |
| External plugin crashes mid-stream | partial sync | the last persisted cursor is authoritative, so a crash loses no committed work and the next round resumes from it |

## Open questions

1. Embedding batch size / concurrency defaults per provider.
2. Graph-walk scoring constants (per-hop proximity decay 0.6, confidence floor
   0.3, RRF k = 60) and the `event_window` default (30d) — the values in
   [05](05-query-engine.md) are the starting point, to be tuned against a real
   workspace rather than a fixture.
3. Whether `impact_of` should also accept an explicit `until` (bounded impact
   windows for "what happened in the following quarter").
4. Jira Data Center auth mode (PAT) — same plugin package when it lands.
5. `lore ask` conversational follow-ups (thread context); today every query is
   single-shot.
