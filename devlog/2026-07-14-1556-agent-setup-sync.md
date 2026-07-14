# 2026-07-14 15:56 — agent-setup sync

Synced the managed AGENTS.md conventions to the current agent-setup
canonical text (user-directed sync; update mode).

## Decisions

- Four blocks had drifted and were synced verbatim: `devlog` (labeled
  drain-record forms), `finish-line` (work contract, no-self-selection
  default, explicit-base checklist, base-freshness handoff),
  `branches` (break-down-concurrency-first + isolate-concurrent-units
  replace the worktree-preference paragraph), `pull-requests`
  (integration-evidence-per-base-commit, validate-against-current-base,
  declared-stacked-PR wording). `context`/`commits`/`done` were already
  in sync. Chose verbatim canonical over local wording: the blocks are
  managed; local nuance belongs outside markers.
- `devlog/README.md` refreshed from the current template: it was a
  strict older version (one hunk, no local customization) and the
  freshly synced devlog block references the labeled escalation forms
  the old copy lacked. The project's existing Deferral escalation
  section already practices those forms, so no behavior change.
- PR template drift is intentional: the local `## Scope` section
  implements Monorepo scope discipline; kept, not "fixed".
  CONTRIBUTING.md and CLAUDE.md match their templates.
- The canonical finish-line's "self-selection requires an explicit
  project-specific opt-in policy" is satisfied here by the existing
  Coordination section (scheduling/fiat doors); no conflict, nothing
  added.

## Verification

- Repo-settings audit: merge-commit-only, title-only merge messages,
  and auto-delete were already aligned; `deferral`/`needs-human` labels
  exist. Branch protection state is unreadable on this plan (GitHub 403:
  private repo without Pro), so required-check strictness stays governed
  by the manual base-freshness procedure the synced text now carries.

## Deferred

- `allow_update_branch` (Always suggest updating pull request branches)
  was off; enabling it was denied by the session permission classifier,
  making it a maintainer-only repo-settings action. -> Refs #54, then
  drained same-session: the user fiat-assigned #54 back to this session,
  the retried PATCH succeeded under that explicit authorization, and an
  independent fresh read confirms `allow_update_branch: true` (the
  issue's acceptance criterion). This PR carries the close keyword.

## To promote

- None this session (the sync itself is the promotion mechanism).
- Queue: grepped the open `## To promote` / deferred / needs-human
  queue. The two open items (`approved-recipe-boundary` invariant,
  `restrict-internal-writes` write-path capability candidate) are
  daemon trust-boundary invariants gated on recurrence/spine review,
  outside this repo-wide-scaffold unit's scope and within their drain
  clocks; neither drained, no re-defer needed. `scaffold-phase0`'s
  devlog-contract item is already marked resolved.
