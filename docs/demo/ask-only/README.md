# Ask-only demo seed data

Six fictional artifacts — three Jira issues and three Notion pages — that make one
decision trail resolvable across two sources with **no repository and no commits**.
They mirror the corpus the automated end-to-end suite already exercises
(`test/e2e/testdata/askonly/`), so a walkthrough on this data shows the same
chains the suite asserts.

Read the walkthrough first: [`docs/demo-ask-only.md`](../../demo-ask-only.md).

## Seeding is a manual step in your own sandbox

Lore is **read-only toward every external source**: it never writes to Jira or
Notion, and this repository ships no script that does. Create these artifacts by
hand, in your own throwaway Jira project and your own Notion page tree. Do not
seed a shared workspace — the tickets are deliberately nonsense and the people in
them do not exist.

## Artifacts

| File | Lives in | Becomes | Links out to |
| --- | --- | --- | --- |
| [`INC-201.md`](INC-201.md) | Jira project `INC` | `jira:ticket:INC-201` | nothing — it is the trail's end |
| [`ARCH-88.md`](ARCH-88.md) | Jira project `ARCH` | `jira:ticket:ARCH-88` | `INC-201` (ticket key in the body) |
| [`OPS-410.md`](OPS-410.md) | Jira project `OPS` | `jira:ticket:OPS-410` | the decision page (URL in the body) |
| [`notion-engineering-decisions.md`](notion-engineering-decisions.md) | Notion | `notion:page:<id>` | nothing; it is the sync root |
| [`notion-checkout-reliability.md`](notion-checkout-reliability.md) | Notion | `notion:page:<id>` | nothing; it is a container |
| [`notion-adopt-option-b.md`](notion-adopt-option-b.md) | Notion | `notion:page:<id>` | `INC-201` (ticket key **and** browse URL) |

The cross-source hop that matters is `OPS-410 → Notion decision page → INC-201`:
a Jira ticket, a Notion page and a Jira ticket in one chain. No single source
holds it.

## Order to create them in

Jira and Notion both stamp `created` themselves — you cannot backdate an issue or
a page from the UI. So the fixture dates below are a *relative* schedule, and what
you reproduce is the order, not the timestamps:

| Order | Artifact | Fixture date | Offset |
| --- | --- | --- | --- |
| 1 | `INC-201` | 2024-06-03 | day 0 |
| 2 | `ARCH-88` | 2024-06-05 | +2 days |
| 3 | Notion `Engineering Decisions` → `Checkout Reliability` → `Adopt option B for checkout writes` | 2024-06-07 | +4 days |
| 4 | `OPS-410` | 2024-06-19 | +16 days |

Chronology is load-bearing for `lore impact`: it walks forward from the anchor's
`created_at` and reports only what is dated strictly after it. Create the decision
page before `OPS-410` or the timeline comes back empty.

## The one reference that needs care

A ticket key resolves by key: writing `INC-201` anywhere in a body is enough, and
capitalization is the only requirement (`[A-Z][A-Z0-9]+-\d+`).

A URL reference resolves by **exact string equality** against the URL the source's
API reported for the target document — trailing `.,;:!?` is trimmed, nothing else
is normalized. A link copied out of the Notion app may carry a `?pvs=…` query
string that the API's own `url` field does not, and then the `OPS-410 → decision
page` edge never forms. The reliable procedure:

1. Create the three Notion pages, then `lore sync`.
2. Read the page's canonical URL out of the index:
   `lore trace "notion:page:<page id>"` prints it under the anchor.
3. Paste *that* URL into `OPS-410`.
4. `lore sync` again.

A reference whose target is not indexed yet is not lost: it is recorded as pending
and retried at the end of every later sync round. Only a *wrong* URL stays broken.
