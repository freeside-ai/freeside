# Work-contract persistence and comment-lease claiming (#59)

`kind:contract` unit, fiat-assigned (user directive). Declared paths:
AGENTS.md, `.github/ISSUE_TEMPLATE/`, `devlog/`. Serialization check:
open contract units #28 and #22 are unmilestoned deferrals with no
active claim, dormant per session-start rule 3; no blockers. Dependency:
the free-skills work-contract kernel is merged (free-skills #67,
`1ff22b1`); Freeside's managed blocks already carry it (synced in #56),
and the comparator ran clean before and after this change, so no shared
prose moved and none was copied into the unmanaged Coordination section.

## Decisions

- **Chose an issue-comment lease over the empty-`Claim #N`-commit draft
  PR** (2026-07-13-1208-coordination-scaffold's mechanism, superseded
  here by user directive): the empty commit forced a claim artifact into
  git history that could never merge and had to be rewritten away, and a
  claim-stage draft PR advertised occupancy only to PR listings. The
  lease keeps the claim on the unit itself (the issue), is versioned
  (`freeside-work-claim:v1` / `freeside-work-release:v1`) and
  mechanically greppable, and resolves contention deterministically
  (earliest `created_at`, numeric comment-ID tie-break). Rejected: claim
  bot, GitHub Project, label state machine (user non-goals; all add a
  service or mutable shared state where a comment log suffices).
- **The open PR with the close keyword stays the terminal claim state**:
  the lease is a 48h bootstrap for the pre-PR window the draft PR used
  to cover; once the PR exists the previous protocol's semantics resume
  unchanged (close unmerged releases, merge closes the issue).
- **Persistence model recorded in Work units**: every unit carries the
  finish line's lightweight contract; direct session-contained
  assignments carry it in prompt + devlog + PR; scheduled, backlog,
  multi-session, and multi-agent work require an issue; a direct task
  crossing one of those boundaries is promoted before continuing.
  Scheduled self-selection is unchanged (user non-goal: no broadening
  or removal). The canonical prose stays only in the managed
  finish-line block; Coordination references it.
- **This unit filed as `kind:contract`** because coordination-scaffold
  records the work-unit form as the 1B elaborator's output contract
  ("changing the form's fields is itself a contract change"). Issue #59
  carries no milestone: it is fiat work, and milestone-without-listing
  is a spine-repair error per Work units.
- **Claimed #59 with the new lease, not a legacy empty commit**: the
  migration rule (no new empty claim commits) applies from this change,
  and the live claim doubles as scenario 1 below.
- **Releases bind to the claim comment's numeric ID, not the branch
  name** (Codex P2 on PR #60): concurrent claimants following the slug
  convention can choose the same branch, so a branch-keyed release from
  the loser would free the winner's claim too; author-scoping fails the
  same way here because concurrent agent sessions share one GitHub
  login. Comment IDs are unique, and the loser knows its own from the
  post response. The branch-keyed PR-transition and expiry rules are
  not the same class: git refs are unique per repo, and adjudication
  precedes branch creation, so only the winner ever holds the branch.
- **Accepted limitation**: comment `created_at` ordering trusts
  collaborator comments; adversarial comment editing is outside the
  threat model (stated in Claiming).

## Verification (state-machine walkthrough)

Each scenario: inputs -> governing rule -> outcome.

1. **First claimant wins** (exercised live on #59): one non-expired,
   unreleased claim comment (id 4974078317) after paginated re-read ->
   earliest-wins -> this session proceeds.
2. **Two near-simultaneous comments**: both re-read all comments ->
   earlier `created_at` wins; identical timestamps -> lower numeric
   comment ID wins; loser posts `freeside-work-release:v1` bound to its
   own claim's comment ID and stops. Both claimants choosing the same
   conventional branch name changes nothing: the release frees only the
   claim it names.
3. **Explicit release**: release comment whose `Releases-claim:` names
   the claim's comment ID -> exactly that lease is released -> unit
   claimable again by anyone.
4. **48h pre-PR expiry**: claim comment older than 48h, no open PR from
   the branch with the close keyword -> lease dead -> re-claim needs a
   new comment (the old comment never revives).
5. **Comment -> open PR** (exercised live at this unit's PR-open): open
   PR from the claimed branch with `Closes #59` -> PR becomes the active
   claim, lease subsumed, no further expiry.
6. **PR closed unmerged**: active-claim PR closed -> claim released ->
   next claimant starts at scenario 1 with a fresh lease.
7. **Merged PR**: close keyword fires -> issue closes -> no claim state
   remains to release.
8. **API failure**: any comment/PR read or write fails at any step ->
   fail closed -> work does not begin (or continue past the failed
   step) while claim state is unverifiable.
9. **Legacy claim**: an open PR with a `Claim #N` commit or close
   keyword predating this change -> recognized as the active claim
   during transition -> unit not claimable; the legacy empty commit is
   dropped in that branch's next rewrite. (Checked live: zero open PRs
   at claim time, so no legacy claims existed to honor.)
10. **Direct task turns multi-session**: no-issue work hits a session
    boundary -> promote to a work-unit issue first (Work units), then
    lease-claim it before continuing; the unclaimed no-issue state never
    spans sessions.

Verifying also revealed: repo settings, labels (`deferral`,
`needs-human` present), and scaffolding all clean in the agent-setup
update pass; the PR template's local `## Scope` section remains the one
intentional divergence. LICENSE is still absent (root-signal audit;
maintainer decision, not this unit).

## To promote

- None. Queue swept: the three open items (`approved-recipe-boundary`
  -> tracked by #52; `domain-package` conventions -> tracked by #27; the
  2026-07-14-1519 store write-boundary candidate, same-day and
  code-scoped) are all outside this unit's declared paths; none drained,
  no re-defer needed.
