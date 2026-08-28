# Lore — Repository Working Agreement

Applies to every contributor, human or agent. The design documents in
`docs/v3/` are the source of truth for architecture and behavior; when code
and design disagree, fix one of them through the same workflow below — never
let them drift silently.

## Branch → PR workflow (no direct commits to `main`)

`main` is protected by convention: it only moves by merging a pull request.

1. **Branch off `main`** for every change, however small:
   - `feat/<short-topic>` — new functionality
   - `fix/<short-topic>` — bug fixes
   - `docs/<short-topic>` — documentation only
   - `chore/<short-topic>` — tooling, CI, dependencies
2. **One logical change per branch.** If a task grows a second concern, split
   it into a second branch/PR.
3. **Commit locally as you work.** Small, atomic commits; imperative subject,
   ≤ 50 characters. Agent-authored commits are prefixed `[AI] `.
4. **Open a PR** when the branch is complete and verified. PR description
   states: what changed, why, how it was verified, and anything left in a
   non-default state. Never claim a skipped check passed.
5. **Merge via squash** to keep `main` linear and readable. Delete the branch
   after merge.

Direct pushes to `main` are reserved for repository initialization only.

## Verification gate (before opening any PR)

- `make build`, `make test`, `make lint` all green locally.
- New behavior carries tests; security-relevant paths test the deny case too.
- Anything unverifiable (missing sandbox credentials, external services) is
  named explicitly in the PR description.

## Repository rules

- **`docs/lore/plans/` is git-ignored working state.** Never commit it,
  never reference a plan file, task ID, or phase number from code, comments,
  tests, commit messages, or repo docs.
- **`docs/v2/` is abandoned research** — git-ignored, not a basis for
  anything.
- **Secrets never enter the repo**: no tokens in code, config, fixtures, or
  commit history. Configuration references environment variable *names* only.
  Test fixtures use obviously fake credentials.
- **Read-only toward external sources.** Lore never writes to GitHub, Notion,
  Jira, or any connected system; tests point at fixtures or disposable
  sandboxes, never shared or production resources.
- Dependencies are limited to the project's agreed allowlist; adding one is
  its own reviewed change with justification.

## Layering (enforced in review)

Transport → Service → Repository/Connector, strictly one-directional
(`docs/v3/02-architecture.md`). Transports never touch the store or a
connector directly. Connector packages import entities and the standard
library only.
