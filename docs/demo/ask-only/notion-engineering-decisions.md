# Notion page — `Engineering Decisions`

The sync root. Fictional; create it by hand in your own Notion workspace. Lore
never writes to Notion.

| Field | Value |
| --- | --- |
| Title | `Engineering Decisions` |
| Parent | top level of the workspace |
| Created | 2024-01-04 in the fixtures; any date before the incident is fine |

## Body

One toggle block, which holds the rest of the tree:

```text
▸ Subsystems
```

Nothing else. This page carries no decision text; it exists to be the single
`sources.notion.root_pages` entry that scopes the sync.

## Why the toggle

The child page below it sits inside a *block*, not directly under a page. Scoping
walks a page's ancestors upward through both page and block parents until it
reaches a root, so a decision buried in a toggle is still in scope. Keeping one
here means the demo exercises that walk instead of assuming a flat tree.

## Setup

1. Create the page.
2. Share it with your Notion integration ("Add connections" → your integration).
   Descendants inherit the share; the root is the only page you have to grant.
3. Put its id in `lore.yaml`:

```yaml
sources:
  notion:
    token_env: LORE_NOTION_TOKEN
    root_pages:
      - 1f2e3d4c5b6a47788990aabbccddeeff   # this page's id
```

An entry may be a page id — dashed or undashed, both are accepted — or an exact
page title. A title that matches two live pages is refused with both ids, and it
asks you to configure the page by id instead.

## What lore stores

| Field | Value |
| --- | --- |
| Document id | `notion:page:<dashed page id>` |
| Title | `Engineering Decisions` |
| Author | *empty* — the Notion connector does not index page authors |
| URL | the page's canonical `notion.so` URL |
| Type | `page` |

The container pages are indexed as documents in their own right, so a real sandbox
reports more documents than the four the fixture corpus trims to. Body-less
container pages cite nothing and link nothing; they just sit in the count.
