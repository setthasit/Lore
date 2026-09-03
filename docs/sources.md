# Sources

Per-source setup: what each connector reads, the smallest credential that lets
it read, and the command that proves the credential works.

Lore is **read-only toward every external source**. No source connector
package contains a write verb: GitLab and Jira are `GET`-only, and the only
non-GET requests among the four source connectors are GitHub's GraphQL
*query* POST (`internal/connectors/github/client.go:138`) and Notion's
`POST /v1/search` (`internal/connectors/notion/client.go:212`), both read
endpoints. There is no `mutation`, `PUT`, `PATCH` or `DELETE` in any source
connector. (The embedder and LLM clients do POST — to their model
endpoints, through the shared `post()` helper,
`internal/connectors/httpretry/httpretry.go:78-79` — but that is the
outbound traffic [Fully local](fully-local.md) covers, not a source
connector writing back to a provider.)

See also: [Quickstart — MCP](quickstart-mcp.md) ·
[Fully local](fully-local.md) ·
[04 — Connectors & Sync](v3/04-connectors-and-sync.md) ·
[06 — Security posture](v3/06-interfaces-and-config.md#security-posture).

## At a glance

| Source | `lore.yaml` key | Environment variables (names only) | Ingest scope | DocTypes produced |
|---|---|---|---|---|
| GitHub | `sources.github` | `LORE_GITHUB_TOKEN` | `repos: ["owner/name"]` | `commit`, `pr`, `pr_review`, `review_comment`, `issue`, `issue_comment` |
| GitLab | `sources.gitlab` | `LORE_GITLAB_TOKEN` | `projects: ["group/project"]` | `commit`, `pr`, `pr_review`, `review_comment`, `issue`, `issue_comment` |
| Notion | `sources.notion` | `LORE_NOTION_TOKEN` | `root_pages` subtrees; empty = every page shared with the integration | `page` |
| Jira | `sources.jira` | `LORE_JIRA_EMAIL`, `LORE_JIRA_TOKEN` | `projects: ["PROJ"]`; empty = every project the account can browse | `ticket`, `ticket_comment` |

Those variable names are only the defaults `lore init` and `lore source add`
suggest. Any name matching `^[A-Za-z_][A-Za-z0-9_]*$` is accepted
(`internal/transport/cli/source.go:23`).

Rules that hold for all four:

- **Every source is optional** (`internal/config/config.go:41-46`). A workspace
  needs at least one source *or* one local clone, else load fails with
  `at least one of sources or repos must be configured`
  (`internal/config/validate.go:25-27`).
- **`*_env` holds a variable name, never a secret.** If the named variable is
  unset, startup stops with
  `sources.<name>.token_env names LORE_<NAME>_TOKEN, but that environment variable is not set`
  — enforced twice, at config validation
  (`internal/config/validate.go:179-186`) and again when the connector is
  constructed (`internal/di/modules.go:200-205`).
- **Unknown keys are rejected**, so a typo is a startup error rather than a
  silently ignored setting: `invalid configuration at ./lore.yaml: …`
  (`internal/config/config.go:167-173`).
- **DocIDs are `<source>:<type>:<external_id>`**
  (`internal/entities/document.go:5-29`), and `Document.URL` is always the
  canonical web URL — the thing a citation points a human at
  (`internal/entities/document.go:40`).
- All examples below use obviously fake placeholders: `acme/myproject`,
  `https://acme.atlassian.net`, `PROJ`. Substitute your own.
- Every claim about *what Lore requests* is read out of this repository and
  cited by `file:line`. Claims about what a provider's scope, permission or
  status code **means** are that provider's documentation, marked **[vendor]**
  — re-check those against the provider's own console, since only Lore's side
  is pinned by tests here.

## GitHub

### What gets ingested

| Thing | DocType | Body | `RepoRef` |
|---|---|---|---|
| Commit on the **default branch** | `commit` | headline + full message | `github:owner/name` |
| Pull request | `pr` | PR body | `github:owner/name` |
| PR review | `pr_review` | review body | `github:owner/name` |
| Review comment | `review_comment` | comment body, with its file path as a ref | `github:owner/name` |
| Issue | `issue` | issue body | `github:owner/name` |
| Issue comment | `issue_comment` | comment body | `github:owner/name` |

Evidence: `internal/connectors/github/connector.go:251-393`; the default-branch
restriction is `defaultBranchRef` in the commits query
(`internal/connectors/github/client.go:524-547`).

### What does not

File contents and diffs (only the *touched path list* of a commit is read),
releases, tags, discussions, wikis, project boards, Actions runs and logs,
deployments, org and user profiles, and branches other than the default. None
of them appears in any query the client issues
(`internal/connectors/github/client.go:524-665, 826-851`).

### Minimum credential

A **fine-grained personal access token**, resource owner = the account or org
that owns the repositories, **Repository access = only the repositories you
list in `repos:`**, and exactly four read-only repository permissions:

| What Lore calls | Where | Permission needed |
|---|---|---|
| `POST /graphql` — every query is rooted at `repository(owner:, name:)` | `client.go:74-79`, `client.go:131-155` | **Metadata: Read-only** (mandatory for every repository endpoint) [vendor] |
| `repository.defaultBranchRef.target.history` — oid, message, dates, author, `changedFilesIfAvailable` | `client.go:524-547` | **Contents: Read-only** [vendor] |
| `GET /repos/{owner}/{repo}/commits/{sha}` — the one REST call, for `filename` + `previous_filename` | `client.go:826-851` | **Contents: Read-only** [vendor] |
| `repository.pullRequests`, `pullRequest.reviews`, review `comments`, `pullRequest.commits`, commit `associatedPullRequests` | `client.go:549-633`, `client.go:540` | **Pull requests: Read-only** [vendor] |
| `repository.issues`, `issue.comments`, `pullRequest.closingIssuesReferences` | `client.go:635-665`, `client.go:562` | **Issues: Read-only** [vendor] |

Paths are relative to `internal/connectors/github/`. The endpoint and field
lists are read out of the repository; the mapping from those to GitHub's
fine-grained permission names is from GitHub's own documentation, marked
**[vendor]** — verify it against your token's permission screen, because
GitHub can retire or rename a permission without Lore noticing.

Nothing beyond those four is used. The token reaches only the `Authorization:
Bearer` header, alongside `Accept: application/vnd.github+json` and
`X-GitHub-Api-Version: 2022-11-28`
(`internal/connectors/github/client.go:197-199`); no code path logs a header,
and error strings carry a status plus the API's own message, never a
credential (`internal/connectors/github/client.go:221`, `:286-309`).

A classic PAT also authenticates — the client only sends a bearer token, so it
cannot tell the two apart — but the narrowest classic scope that reaches
private repositories is `repo`, which is read **and write** across every
repository you can see [vendor]. Prefer fine-grained.

GitHub Enterprise Server: the connector accepts an API root
(`NewConnector(token, repos, baseURL)` — `connector.go:67-79`), but
`lore.yaml` exposes no key for it and the wiring passes an empty string
(`internal/di/modules.go:166`), so today only `github.com` is reachable.

### `lore.yaml`

```yaml
sources:
  github:
    token_env: LORE_GITHUB_TOKEN   # env var NAME; a fine-grained read-only PAT
    repos:
      - acme/myproject             # "owner/name"; no local clone required
      - acme/myproject-infra
```

### `lore source add`

There is nothing to walk through: `lore init` already scaffolds this block
(`internal/transport/cli/init.go:74-80`), so the subcommand refuses the name
and tells you where to edit —

```console
$ lore source add github
lore: `lore init` already scaffolds sources.github — edit that block in lore.yaml; source add takes notion or jira or gitlab
```

exit code 2 (bad request). Verbatim from
`internal/transport/cli/source.go:119-121`; the accepted-name list is built
from the registered specs (`source.go:35-39, 130-136`), so it grows as
sources are added.

### Verify

```console
$ export LORE_GITHUB_TOKEN=ghp_0000000000000000000000000000000000  # fake
$ lore sync --source github
sync complete — `lore status` for counts and cursor ages
$ lore status
documents: 412
chunks:    1180
edges:     903

sources:
  github     last checkpoint just now (2026-09-02T10:12:31Z)

sync lock: free
```

`lore sync --source github` limits the round to that one connector
(`internal/transport/cli/sync.go:41-42`), passed through as
`services.SyncOptions{Source: source}` (`sync.go:25`), so that pair of
commands is the end-to-end proof no matter what else is configured: the
`github` line in `lore status` exists only after a batch committed
(`internal/transport/cli/status.go:36-44`). An unknown name is refused:
`unknown source "<name>"; this workspace has <the configured names>`
(`internal/services/sync.go:177-178`). `--source` cannot be combined with
`--reembed`: `cannot re-embed a single source: a re-embed wipes every
source's chunks and rewinds every cursor, so it must run across the whole
workspace` (`internal/services/sync.go:93-96`). The MCP `sync_now` tool's
`source` field and gRPC `Trigger`'s `source` field are the same filter.

Failure looks like this (exit 1, and the token itself never appears):

```console
$ lore sync
lore: connector github could not read changes: github acme/myproject: POST https://api.github.com/graphql: status 401: Bad credentials
```

The `connector github could not read changes` prefix is Lore's
(`internal/services/sync.go:329-331`), `github acme/myproject` names the
repository being walked (`internal/connectors/github/connector.go:101`), and
the rest is the request line, the HTTP status and GitHub's own message
(`internal/connectors/github/client.go:221`). Read it as:

| Status | Almost always means |
|---|---|
| 401 | token revoked, expired, or the wrong variable exported |
| 403 | permission missing for the field being read, or SSO authorization not granted for the org |
| 404 on `repository` | repository not selected in the token's repository access — a fine-grained PAT hides what it cannot read |

Lore authors the prefix; what each status *means* is the provider's documented
behavior, not something this repository decides [vendor].

A missing variable is caught earlier, before any request:
`sources.github.token_env names LORE_GITHUB_TOKEN, but that environment
variable is not set` (exit 2).

### Rotate and revoke

Fine-grained PATs expire; a sync starts failing with 401 the moment one does.
To rotate, mint the replacement, `export LORE_GITHUB_TOKEN=<new>`, re-run
`lore sync`, then delete the old token. Nothing has to change in `lore.yaml`
— it stores the variable name, not the value — and the index survives, since
cursors are per-source and per-repo, not per-credential
(`internal/connectors/github/connector.go:459-474`). Revoking is enough to
stop all ingestion: with no valid token the connector cannot read, and Lore
has no cached credential anywhere.

## GitLab

Same shape as GitHub, one source name later: a merge request is the PR
analogue, and MR discussion notes are the review analogue. No new DocTypes
were introduced for it.

### What gets ingested

| Thing | DocType | Canonical URL form | `RepoRef` |
|---|---|---|---|
| Commit | `commit` | `<base>/<group>/<project>/-/commit/<sha>` | `gitlab:group/project` |
| Merge request | `pr` | `<base>/<group>/<project>/-/merge_requests/<iid>` | `gitlab:group/project` |
| MR review thread (its opening note) | `pr_review` | MR URL + `#note_<id>` | `gitlab:group/project` |
| MR discussion note | `review_comment` | MR URL + `#note_<id>` | `gitlab:group/project` |
| Issue | `issue` | `<base>/<group>/<project>/-/issues/<iid>` | `gitlab:group/project` |
| Issue note | `issue_comment` | issue URL + `#note_<id>` | `gitlab:group/project` |

`Document.Source` is `"gitlab"` and `RepoRef` is `gitlab:<group>/<project>`,
so a subgroup path (`acme/platform/myproject`) round-trips intact. Each URL is
GitLab's own `web_url` when the payload carries one, falling back to the
constructed form above (`internal/connectors/gitlab/connector.go:172-182,
272, 309, 419`). The sync watermark is GitLab's own filter on each list
endpoint — `updated_after` for merge requests and issues, `since` for commit
history — checkpointed per batch like every other connector.

### What does not

Patch text: the commit `diff` endpoint is called only to learn which files a
commit touched — `new_path`, `old_path` and `renamed_file` are decoded, the
diff body is not, and the paths become `file_path` refs on the commit document
and nothing else. Also excluded: CI/CD pipelines and job logs, snippets,
wikis, releases, epics, container and package registries, group-level objects,
and any project not listed in `projects:`.

### Minimum credential

A token with the **`read_api`** scope — read-only access to the whole v4 API,
and the narrowest scope that covers merge requests, issues, notes and commits
in one grant [vendor]. The block holds **one** `token_env`, so that single
token has to reach every path in `projects:`. Pick the tightest kind that
does:

1. **Project access token** (project → Settings → Access tokens), role
   *Reporter*, scope `read_api`. Reaches that one project and nothing else —
   the least-privilege choice, and the right one when `projects:` names a
   single project.
2. **Group access token** (group → Settings → Access tokens), role *Reporter*,
   scope `read_api`, when every configured path sits under one group. It
   reaches every project in that group, including ones you do not index.
3. **Personal access token**, scope `read_api`, when the paths span groups. It
   reaches everything *you* can read — by far the largest blast radius, so
   prefer a dedicated integration account over your own.

Avoid `api`: it is read **and write**. `read_repository` is not enough on its
own — it covers git and repository files, not the merge requests, issues and
notes that carry the decisions [vendor].

Seven read endpoints, all `GET`, all under `<base_url>/api/v4`, with the
project addressed by its URL-encoded namespaced path (`acme%2Fmyproject`):

| What Lore calls | Params it sends | Where | Covered by |
|---|---|---|---|
| `GET /projects/:path/repository/commits` | `since` (only when resuming), `per_page`, `page` | `client.go:420-425` | `read_api` [vendor] |
| `GET /projects/:path/repository/commits/:sha/diff` | `per_page`, `page` | `client.go:432-434` | `read_api` [vendor] |
| `GET /projects/:path/merge_requests` | `order_by=updated_at`, `sort=asc`, `updated_after` (only when resuming), `per_page`, `page` | `client.go:388-393` | `read_api` [vendor] |
| `GET /projects/:path/merge_requests/:iid/discussions` | `per_page`, `page` | `client.go:400-402` | `read_api` [vendor] |
| `GET /projects/:path/merge_requests/:iid/commits` | `per_page`, `page` | `client.go:409-411` | `read_api` [vendor] |
| `GET /projects/:path/issues` | `order_by=updated_at`, `sort=asc`, `updated_after` (only when resuming), `per_page`, `page` | `client.go:441-446` | `read_api` [vendor] |
| `GET /projects/:path/issues/:iid/notes` | `order_by=created_at`, `sort=asc`, `per_page`, `page` | `client.go:453-455` | `read_api` [vendor] |

Paths are relative to `internal/connectors/gitlab/`. `per_page=100` and the
`page` cursor are set by the shared pager (`client.go:348-358`), which follows
GitLab's `X-Next-Page` response header and treats a full page as evidence of
another one when GitLab omits the pagination headers
(`client.go:369-383`). The base is `<root>/api/v4` (`client.go:34, 41`), and
the token is sent as the `PRIVATE-TOKEN` request header only
(`client.go:37-38, 109`) — never as a query parameter, so it cannot leak into
a server access log.

### `lore.yaml`

```yaml
sources:
  gitlab:
    base_url: https://gitlab.com   # OPTIONAL — default https://gitlab.com;
                                   # a self-managed instance passes its root
    token_env: LORE_GITLAB_TOKEN   # env var NAME; a read_api token
    projects:                      # namespaced paths, at least one
      - acme/myproject
      - acme/platform/myproject    # subgroups are fine
```

Validation at config load — the first rule broken is the one reported, before
any request goes out (exit 2 — `internal/config/validate.go:71-83`):

| Wrong | Message |
|---|---|
| `token_env` absent | `sources.gitlab.token_env must name an environment variable` |
| the named variable unset | `sources.gitlab.token_env names LORE_GITLAB_TOKEN, but that environment variable is not set` |
| `projects` empty | `sources.gitlab.projects must list at least one "group/project" path` |
| `base_url` not absolute http(s) | `sources.gitlab.base_url must be an absolute http(s) URL like https://gitlab.com, got ftp://gitlab.example.com` |
| `base_url` unparseable | `sources.gitlab.base_url is not a URL: <value>` |

Unlike Jira, `base_url` is optional here: absent means `https://gitlab.com`.
A project entry that is not a `group/project` path is caught one step later,
by the connector rather than the loader:
`gitlab: invalid project "myproject": want "group/project"`
(`internal/connectors/gitlab/connector.go:151-153`).

### `lore source add gitlab`

```console
$ lore source add gitlab
GitLab base URL [https://gitlab.com]: 
name of the environment variable holding the GitLab access token — the name, never the value [LORE_GITLAB_TOKEN]: 
GitLab projects to sync, comma-separated, e.g. acme/myproject or acme/platform/myproject: acme/myproject, acme/platform/myproject
added sources.gitlab to ./lore.yaml
next: export LORE_GITLAB_TOKEN, then run `lore sync`
```

Prompts verbatim from `internal/transport/cli/source.go:279-307`. Both
bracketed defaults are taken by pressing Enter; the projects question is not
optional — an empty answer stops there with
`sources.gitlab.projects must list at least one entry` (exit 2,
`source.go:198-209`), which is the same rule config load enforces later with
its own wording. The credential is never typed: the prompt asks for the *name*
of the variable holding it (`source.go:160-170`).

`base_url` is written out even when you accept the default, so a self-managed
instance is a visible edit rather than an invisible assumption
(`source.go:25-27`).

What lands in `lore.yaml`, spliced in under the existing `sources:` line with
every other line left untouched (`internal/transport/cli/source.go:339-352`):

```yaml
  gitlab:
    base_url: https://gitlab.com
    token_env: LORE_GITLAB_TOKEN
    projects:
      - acme/myproject
      - acme/platform/myproject
```

Adding a source twice is refused rather than merged:
`sources.gitlab is already configured in ./lore.yaml — edit that block
directly` (exit 3 — `internal/transport/cli/source.go:85-87`).

### Verify

```console
$ export LORE_GITLAB_TOKEN=glpat-0000000000000000000000  # fake
$ lore sync
sync complete — `lore status` for counts and cursor ages
$ lore status
documents: 128
chunks:    377
edges:     205

sources:
  gitlab     last checkpoint just now (2026-09-02T10:41:07Z)

sync lock: free
```

Failure carries the same three parts as GitHub — Lore's prefix
(`internal/services/sync.go:329-331`), the project being walked
(`internal/connectors/gitlab/connector.go:110`), then the request line, status
and GitLab's own message (`internal/connectors/gitlab/client.go:129`,
`:196-213`):

```console
$ lore sync
lore: connector gitlab could not read changes: gitlab acme/myproject: GET https://gitlab.com/api/v4/projects/acme%2Fmyproject/merge_requests?order_by=updated_at&page=1&per_page=100&sort=asc: status 401: 401 Unauthorized
```

Any URL echoed that way is scrubbed of `private_token` and `access_token`
query parameters first (`internal/connectors/gitlab/client.go:179-194`) —
belt and braces, since Lore only ever sends the header form.

| Status | Almost always means |
|---|---|
| 401 | token revoked, expired, or the wrong variable exported |
| 403 | scope too narrow — `read_repository` instead of `read_api`, or a role below Reporter |
| 404 on a project path | the token cannot see that project; GitLab answers 404 rather than 403 for invisible projects [vendor], so check the path spelling *and* the token's project |

The status column is GitLab's documented behavior [vendor]; only the message
prefix and the request line come from this repository.

### Rotate and revoke

Project, group and personal access tokens all carry an expiry, and a sync
starts failing with 401 the moment one lapses. Mint the replacement,
`export LORE_GITLAB_TOKEN=<new>`, run `lore sync`, revoke the old one. Since
the block names one variable, revoking the token stops GitLab ingestion
entirely — narrowing to a subset means shortening `projects:` (or splitting
the workspace), not juggling several tokens.

## Notion

### What gets ingested

Pages only — one `page` document per page, its title from whichever property
has type `title`, its body the page's block tree flattened to text
(`internal/connectors/notion/connector.go:153-185`). Blocks are walked
recursively but a `child_page` block is not descended into, because that page
arrives as its own document (`internal/connectors/notion/client.go:236-240`).
Trashed pages are skipped (`connector.go:113`,
`internal/connectors/notion/client.go:109-110`).

Notion documents carry **no** `RepoRef` and **no** `Author`
(`internal/connectors/notion/connector.go:174-184`) — the connector never
asks Notion who anybody is.

### What does not

Databases and data sources as objects (a database *parent* just ends the
ancestor walk — `client.go:87-96`), page comments, users, files and
attachments, and any page not shared with the integration: `/v1/search`
returns only what the integration can see.

### Minimum credential

An **internal integration** in your own workspace
(Notion → Settings → Connections → integrations), with:

- **Read content** capability, and nothing else. The connector reads pages and
  blocks only; it never reads comments, never reads user information, and
  issues no update or insert call [vendor: capability names].
- **Access explicitly granted to the pages you intend to index**, and only
  those. In Notion a page share cascades to its subtree, so sharing one
  parent page is usually the whole grant.

| What Lore calls | Where | Why it needs the share |
|---|---|---|
| `POST /v1/search` with `filter: {property: object, value: page}`, sorted by `last_edited_time` ascending | `client.go:197-226` | returns exactly the pages shared with the integration |
| `GET /v1/blocks/{id}/children?page_size=100` | `client.go:248-267` | the page body |
| `GET /v1/pages/{id}` | `client.go:269-275` | confirms a `root_pages` id and walks a page parent |
| `GET /v1/blocks/{id}` | `client.go:285-291` | walks a block parent when a page is nested inside a block |

Paths are relative to `internal/connectors/notion/`. Every request sends
`Authorization: Bearer` plus `Notion-Version: 2026-03-11`
(`client.go:21`, `client.go:351-352`).

`root_pages` is a **filter, not a grant**: it narrows an already-granted
subtree. Entries may be page ids or exact page titles
(`connector.go:67-69, 218-248`); a page is in scope when it *is* a root or
has one as an ancestor, found by walking parents up to 32 levels
(`connector.go:22-23, 261-297`). Consequences worth knowing before you rely
on it:

- An **empty `root_pages` syncs every page shared with the integration**
  (`connector.go:262-264`). The share list is then your only boundary.
- A title that matches two live pages is an error, not a guess:
  `notion: root page "Decisions" matches the live pages <id> and <id>: configure it by id`
  (`connector.go:246-247`). Ids are the durable choice — a renamed page breaks
  a title entry with `notion: root page "Decisions" matches no page title`
  (`connector.go:242`).
- Dashes and case in ids do not matter (`connector.go:299-302`).

Notion's API host is not configurable: the wiring passes an empty base URL and
the client defaults to `https://api.notion.com`
(`internal/di/modules.go:182`, `internal/connectors/notion/client.go:18, 52-55`).

### `lore.yaml`

```yaml
sources:
  notion:
    token_env: LORE_NOTION_TOKEN
    root_pages:
      - 00000000000000000000000000000000   # page id (fake)
      - Architecture Decisions             # or an exact page title
```

### `lore source add notion`

```console
$ lore source add notion
name of the environment variable holding the Notion integration token — the name, never the value [LORE_NOTION_TOKEN]: 
Notion root pages to scope the sync to, comma-separated (empty syncs every page shared with the integration): 00000000000000000000000000000000, Architecture Decisions
added sources.notion to ./lore.yaml
next: export LORE_NOTION_TOKEN, then run `lore sync`
```

Prompts verbatim from `internal/transport/cli/source.go:229-244`. Pasting a
token at the first prompt is rejected without echoing it back:
`sources.notion.token_env must be an environment variable name like
LORE_NOTION_TOKEN` (`source.go:165-168`). Written block:

```yaml
  notion:
    token_env: LORE_NOTION_TOKEN
    root_pages:
      - "00000000000000000000000000000000"
      - Architecture Decisions
```

The encoder quotes an all-digit page id, as above, so it stays a string;
`root_pages` is omitted entirely when you answer the second prompt with an
empty line (`omitempty` — `source.go:211-214`).

### Verify

```console
$ export LORE_NOTION_TOKEN=ntn_000000000000000000000000000000000000000000  # fake
$ lore sync
sync complete — `lore status` for counts and cursor ages
$ lore status
…
sources:
  notion     last checkpoint just now (2026-09-02T10:44:52Z)
```

Two distinct failures are worth telling apart, because only one of them looks
like an error:

```console
$ lore sync
lore: connector notion could not read changes: notion: POST https://api.notion.com/v1/search: status 401: API token is invalid
```

`connector notion could not read changes` is Lore's
(`internal/services/sync.go:329-331`), `notion:` is the connector's
(`internal/connectors/notion/connector.go:141-145`), and the tail after
`status 401:` is Notion's own message [vendor]
(`internal/connectors/notion/client.go:374, 417-431`).

The second failure does not look like one: a sync that succeeds and indexes
**nothing**.

```console
$ lore status
…
sources: none have checkpointed yet — run `lore sync`
```

That is the signature of a valid token whose integration was never shared with
any page — `/v1/search` answers 200 with an empty result set, no batch is
committed, so no cursor is written (`internal/transport/cli/status.go:36-37`).
Fix it in Notion (page → ⋯ → Connections → your integration), not in
`lore.yaml`.

A `root_pages` entry that resolves to nothing is a hard error instead:
`notion: root page "Decisions" matches no page title`.

### Rotate and revoke

Internal integration tokens do not expire on their own but can be rotated from
the integration's settings page. Mint, `export LORE_NOTION_TOKEN=<new>`,
`lore sync`, revoke the old. Two levers exist for narrowing access after the
fact: revoke the token (all ingestion stops), or un-share a page from the
integration (that subtree stops being read; documents already in the index
stay until you rebuild it — the index is derived data and safe to delete).

## Jira

Jira **Cloud** only. The connector calls `/rest/api/3/search/jql`
(`internal/connectors/jira/client.go:268-277`), an endpoint Jira Server and
Data Center do not expose, so this connector does not support them [vendor].

### What gets ingested

| Thing | DocType | Title | URL |
|---|---|---|---|
| Issue | `ticket` | `PROJ-123: <summary>` | `<base_url>/browse/PROJ-123` |
| Comment | `ticket_comment` | `Comment on PROJ-123` | `<base_url>/browse/PROJ-123?focusedCommentId=<id>` |

Only five fields are requested per issue — `summary,description,created,updated,reporter`
(`internal/connectors/jira/client.go:35`) — plus its comments. Description and
comment bodies arrive as Atlassian Document Format and are flattened to plain
text (`connector.go:193, 209`; `internal/connectors/jira/adf.go`). Jira
documents carry no `RepoRef` (`connector.go:221-227`).

The bare issue key is the document's external id on purpose: it is what makes
a `PROJ-123` mention in a commit, PR or Notion page resolve to this ticket
(`connector.go:189-191`).

### What does not

Every other field: status, assignee, labels, components, sprints, story
points, custom fields, attachments, worklogs, changelog/history, transitions,
watchers, and issue links. Also no boards, no projects metadata, no users.
Nothing outside `projects:` is queried, because the project filter is a JQL
clause (`connector.go:233-245`).

### Minimum credential

An **Atlassian API token** (id.atlassian.com → Security → API tokens) for a
user account, sent as HTTP basic auth — `base64(email + ":" + token)`
(`internal/connectors/jira/client.go:73-83`). Jira has no read-only token type
[vendor]: **the token inherits every permission of the account it belongs to**,
so least privilege here is a property of the *account*, not of the token. Use a
dedicated integration user whose only relevant grant is:

- **Browse Projects** on exactly the projects listed in `projects:`, granted
  through a permission scheme, and no add/edit/transition/delete permission
  anywhere [vendor: permission name].

| What Lore calls | Where | Why that permission |
|---|---|---|
| `GET /rest/api/3/search/jql?jql=…&fields=summary,description,created,updated,reporter&maxResults=50` | `client.go:265-290` | JQL search returns only issues in projects the account can browse |
| `GET /rest/api/3/issue/{key}/comment?startAt=…&maxResults=100&orderBy=created` | `client.go:294-314` | comments of a browsable issue |

Paths are relative to `internal/connectors/jira/`. Both are GET; the
connector issues no other request. The JQL it builds is
`project IN (PROJ, PLATFORM) AND updated >= "<watermark - 24h>" ORDER BY updated ASC`
(`connector.go:26-28, 233-245`) — the 24-hour slack absorbs JQL's
minute-granular, requester-timezone datetime literals, and the connector
refilters the overlap.

Two scoping cautions:

- **An empty `projects` list ingests every project the account can browse**
  — `jql()` emits no `project IN` clause without it (`connector.go:235`).
  List the projects explicitly; that way the config, not the account's
  permission scheme, is the boundary you review.
- Project keys are validated before any request:
  `jira: invalid project key "proj-1": want uppercase letters, digits and underscores`
  (`connector.go:247-267`).

### `lore.yaml`

```yaml
sources:
  jira:
    base_url: https://acme.atlassian.net   # REQUIRED
    email_env: LORE_JIRA_EMAIL             # env var NAME holding the account email
    token_env: LORE_JIRA_TOKEN             # env var NAME holding the API token
    projects:
      - PROJ
      - PLATFORM
```

`base_url` is required outright here — unlike GitLab's, it has no default,
because there is no canonical Jira host:
`sources.jira.base_url must be set` (`internal/config/validate.go:89-92`). The
email is treated as a credential too — it is half of the basic-auth pair, so
it also lives in an environment variable, and both variables must be set
before startup (`internal/config/validate.go:93-98`).

### `lore source add jira`

```console
$ lore source add jira
Jira base URL, e.g. https://acme.atlassian.net: https://acme.atlassian.net
name of the environment variable holding the Jira account email — the name, never the value [LORE_JIRA_EMAIL]: 
name of the environment variable holding the Jira API token — the name, never the value [LORE_JIRA_TOKEN]: 
Jira project keys to sync, comma-separated (empty syncs every project the account can see): PROJ, PLATFORM
added sources.jira to ./lore.yaml
next: export LORE_JIRA_EMAIL and LORE_JIRA_TOKEN, then run `lore sync`
```

Prompts verbatim from `internal/transport/cli/source.go:246-277`. The base URL
is checked on the spot:
`sources.jira.base_url must be an absolute http(s) URL like https://acme.atlassian.net, got acme.atlassian.net`
(`source.go:309-318`). Written block:

```yaml
  jira:
    base_url: https://acme.atlassian.net
    email_env: LORE_JIRA_EMAIL
    token_env: LORE_JIRA_TOKEN
    projects:
      - PROJ
      - PLATFORM
```

### Verify

```console
$ export LORE_JIRA_EMAIL=lore-bot@example.com
$ export LORE_JIRA_TOKEN=ATATT0000000000000000000000  # fake
$ lore sync
sync complete — `lore status` for counts and cursor ages
$ lore status
…
sources:
  jira       last checkpoint just now (2026-09-02T10:47:19Z)
```

Failure:

```console
$ lore sync
lore: connector jira could not read changes: jira: GET https://acme.atlassian.net/rest/api/3/search/jql?fields=…&jql=…: status 401: <the API's own message>
```

| Status | Almost always means |
|---|---|
| 401 | wrong email/token pair — both halves come from the two variables, so check that the email matches the account that minted the token |
| 403 | the account exists but is not allowed to search; also what a captcha-locked account returns [vendor] |
| 400 | JQL rejected — most often a project key the account cannot browse, which Jira reports as an unknown project [vendor] |

The status column is Atlassian's documented behavior [vendor].

A JQL/permission mistake is therefore *not* silent: unlike Notion, a
misconfigured project key surfaces as a 400 rather than an empty result set.

### Rotate and revoke

API tokens are revoked per token at id.atlassian.com; revoking one does not
disturb the account's other tokens. Mint, `export LORE_JIRA_TOKEN=<new>`,
`lore sync`, revoke the old. Because the token carries the account's full
permission set, the stronger control is the account: removing its Browse
Projects grant on a project ends ingestion for that project even while the
token stays valid.

## Security posture, all sources

The per-source detail above collapses into five rules that hold everywhere.
The design-level statement lives in
[06 — Security posture](v3/06-interfaces-and-config.md#security-posture); this
is what the code does.

1. **Read-only, structurally.** No source connector package imports anything
   beyond entities, the standard library, the shared ref scanner and the
   shared retry helper, and each issues read requests only
   — GitLab and Jira are `GET`-only, and GitHub's GraphQL query POST and
   Notion's `POST /v1/search` are the sole non-GET calls among the four
   source connectors, both read endpoints. No `mutation`, `PUT`, `PATCH` or
   `DELETE` exists in any source connector. (The embedder and LLM clients do
   POST, to their model endpoints, through the same shared `post()` helper —
   `internal/connectors/httpretry/httpretry.go:78-79` — that's the outbound
   traffic [Fully local](fully-local.md) covers, not a source-connector
   write.) A compromised Lore process cannot edit a merge request, close an
   issue, or write a Notion page, because the capability is absent, not
   merely unused.
2. **Secrets come from the environment, only.** `lore.yaml` stores variable
   *names* (`token_env`, `email_env`, `api_key_env`); the `lore source add`
   prompts ask for the name and reject a value without echoing it
   (`internal/transport/cli/source.go:58-60, 160-170`). A named-but-unset
   variable is a startup error that names the variable to export
   (`internal/config/validate.go:178-187`).
3. **Secrets never reach disk or logs.** A credential is read from the
   environment at connector construction (`internal/di/modules.go:200-205`)
   and lives only in a request header — `Authorization` for GitHub, Notion and
   Jira, `PRIVATE-TOKEN` for GitLab. The index schema has nowhere to put one:
   its tables are `documents`, `chunks`, `edges`, `pending_refs`, `cursors`,
   `sync_lock` and `meta` (`internal/repositories/sqlite/schema.go:19-95`).
   Error strings carry a method, a URL, an HTTP status and the API's own
   message — bounded to 512 bytes so an HTML error page cannot flood a log
   (`internal/connectors/github/client.go:36`,
   `internal/connectors/notion/client.go:35`,
   `internal/connectors/jira/client.go:30`) — and never a header. The GitLab
   connector additionally scrubs `private_token` and `access_token` query
   parameters out of any URL it echoes
   (`internal/connectors/gitlab/client.go:179-194`), because GitLab documents
   that query form even though Lore never uses it.
4. **What a compromised token would expose.** The token's own read scope, not
   Lore's index. Read access to the private prose of your engineering process:
   PR and review discussion, issue and ticket threads, commit messages, and the
   Notion pages shared with the integration. That is what makes the narrow
   grants above worth the effort — a repository-scoped fine-grained PAT, the
   narrowest GitLab token that still reaches every configured path, an
   integration shared with one page subtree, a Jira account that can browse
   only the listed projects. Each one
   caps the damage at the projects you actually index. It is also why the
   `projects` / `root_pages` scoping keys should be filled in explicitly:
   leaving them empty makes the credential's own reach the boundary.
5. **Private data leaves the machine toward exactly one other place:** the
   configured embedder and, if you configure one, the LLM. Every document body
   that gets indexed is sent to the embedder. To keep that on your hardware
   too, see [Fully local](fully-local.md).

Rotation is uniform: mint the new credential, re-export the same variable
name, run `lore sync`, revoke the old one. `lore.yaml` never changes, and the
index survives — cursors are keyed by source, not by credential. Revoking is
always sufficient to stop ingestion: Lore caches no credential anywhere.
