# ARCH-88 — the rejected alternative

Fictional. Create it by hand in your own sandbox Jira project; lore never writes
to Jira.

| Field | Value |
| --- | --- |
| Project key | `ARCH` |
| Issue type | Task |
| Summary | `Option A versus option B for the checkout write path` |
| Reporter | Grace Hopper &lt;grace.hopper@example.invalid&gt; |
| Created | 2024-06-05 10:00 UTC in the fixtures — create it **after** `INC-201` |

## Description

A paragraph, a two-item bullet list, then a closing paragraph. The bullet list is
not decoration: it is the document that holds both options side by side, which is
what makes "X over Y" answerable at all.

```text
INC-201 forced a choice between two options for the checkout write path.

- Option A shards the shared lock per tenant, which keeps writes parallel and needs no new component.
- Option B routes every checkout write through one owning writer, which costs throughput but removes contention.

Option A is rejected: a hot tenant still puts two writers on one row and would reproduce INC-201.
```

## Cross-references

| Reference | How it resolves | Edge |
| --- | --- | --- |
| `INC-201`, twice in the body | ticket key → `jira:ticket:INC-201` | `ARCH-88 → INC-201` |

Writing the key as plain text is enough; no link is needed. The connector drops a
ticket's reference to *itself*, so mentioning `ARCH-88` in its own body does
nothing.

## What lore stores

| Field | Value |
| --- | --- |
| Document id | `jira:ticket:ARCH-88` |
| Title | `ARCH-88: Option A versus option B for the checkout write path` |
| Author | `Grace Hopper` |
| URL | `<sources.jira.base_url>/browse/ARCH-88` |

## Why this ticket is the point of the demo

It is the only artifact that says *why option A lost*. The end-to-end suite
asserts specifically that this document is cited and that it carries a URL, i.e.
that the rejected alternative is quotable back to a human. A trail that answers
"what did we pick" while dropping "what we rejected" is the failure mode this
ticket exists to catch.
