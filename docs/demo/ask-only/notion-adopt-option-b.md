# Notion page — `Adopt option B for checkout writes`

The decision. Fictional; create it by hand in your own Notion workspace. Lore
never writes to Notion.

| Field | Value |
| --- | --- |
| Title | `Adopt option B for checkout writes` |
| Parent | sub-page of [`Checkout Reliability`](notion-checkout-reliability.md) |
| Created | 2024-06-07 in the fixtures — create it **after** both Jira issues `INC-201` and `ARCH-88`, and **before** `OPS-410` |

## Body

An `H2`, two paragraphs, two bullets, one closing paragraph. In the third line,
`INC-201` appears twice: once as plain text and once as a link to the Jira browse
URL.

```text
## Decision

We choose option B over option A: every checkout write goes through one owning writer.

The trigger was INC-201, the checkout write stall. Ticket: [INC-201](https://<your-site>.atlassian.net/browse/INC-201)

- Option A shards the shared lock per tenant and stays rejected.
- Option B serializes every checkout write behind one owning writer.

Written four days after the stall, once the timeline was reconstructed.
```

Substitute your own Jira site for `<your-site>`; it must be the same host as
`sources.jira.base_url`, because that is the host the connector builds the ticket's
stored URL from.

## Cross-references

| Reference | How it resolves | Edge |
| --- | --- | --- |
| `INC-201` as plain text | ticket key → `jira:ticket:INC-201` | `page → INC-201` |
| the `/browse/INC-201` link | URL → the same ticket, by exact URL match | `page → INC-201` |

Both are deliberate. The bare key is the robust path — it resolves by key, so it
survives a mistyped host — and the link is what a human would actually write. The
key alone is sufficient; if you would rather keep the body shorter, drop the link,
not the key.

## What lore stores

| Field | Value |
| --- | --- |
| Document id | `notion:page:<dashed page id>` |
| Title | `Adopt option B for checkout writes` |
| Author | *empty* — the Notion connector does not index page authors, so this node prints as `notion page · <date>` with no author segment |
| URL | the page's canonical `notion.so` URL, e.g. `https://www.notion.so/acme/Adopt-Option-B-d4d4d4d4` in the fixtures |
| Type | `page` |

The indexed body is the block tree flattened to markdown-ish text: `## Decision`,
the paragraphs, `- ` bullets, and the link rendered as
`[INC-201](https://…/browse/INC-201)`. Both the plain key and the link's URL are
scanned out of that text.

## Its two jobs in the walkthrough

1. **Middle of the chain.** `OPS-410 → this page → INC-201` is the chain that no
   single source holds, and the one the end-to-end suite asserts spans both Jira
   and Notion.
2. **Anchor of the second question.** `lore impact` starts from this page and
   walks forward in time, which is how `OPS-410` surfaces as its consequence.

Note that the page must be dated *before* `OPS-410`: `lore impact` reports only
documents created strictly after its anchor.
