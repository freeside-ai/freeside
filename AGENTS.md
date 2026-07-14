# AGENTS.md

**Freeside** is an agent control plane: a local, durable workflow controller that turns a software work item into an evidence-backed pull request and interrupts a human only when judgment is required. The spec, architecture, and roadmap live in [`docs/plan.md`](docs/plan.md); read it first, and argue changes against it. This file holds the development conventions that apply to every session: workflow bookends, branch/PR/commit discipline, and the monorepo's scope rules.

Freeside is a monorepo. Each component directory (`daemon/`, `app/`, `api/`, `prompts/`, `policy/`, `images/`) stays empty until the phase that fills it, holding only a `README.md` stating its purpose until then; the per-component phase lives in that README and the roadmap (`docs/plan.md` §11). Do not scaffold a component ahead of its phase. "Empty" is not uniform: the API is provisional (plan §11 Wave 0; the decision record lives in docs/history/decisions.md), so drafting its skeleton in `api/` as a pre-1A design artifact is in scope, not a scope violation; `app/` starts with Phase 1A's minimal clients; the rest come in Phase 1A or later per their READMEs.

CLAUDE.md is a pointer that imports this file; edit AGENTS.md, never the pointer.

<!-- agents-md:managed:devlog -->

## Devlog (session bookends)

`devlog/` holds the reasoning trail: one short entry per working
session. `devlog/README.md` is the protocol: entry naming, density
target, structure, and when an entry may be revised.

- **Before starting:** read the most recent one or two entries
  (`find devlog -maxdepth 1 -type f -name '*.md' ! -name README.md | sort | tail -2`);
  they carry decisions and deliberate deferrals that aren't in the spec.
  Don't re-litigate or "fix" what an entry marks as decided/deferred without
  the user asking. Also grep the devlog for the open `## To promote` /
  deferred / needs-human queue so promotions don't span sessions unnoticed:
  an item with no `->` state marker (or whose `-> re-deferred` clock has
  run out) is open, unless a later entry or a tracker issue naming that
  item and its source entry holds its drain record (see devlog/README.md).
- **Before finishing:** append `devlog/YYYY-MM-DD-HHMM-slug.md`: decisions
  (why, and what was rejected), deferrals, open questions; the entry may be
  built incrementally at checkpoints while its PR is unmerged (see
  devlog/README.md). Note anything
  that should be promoted to AGENTS.md: a new invariant discovered, a
  convention that wasn't written down, a gotcha that bit you; the entry
  records it, a follow-up commit promotes it. Draining or re-deferring a
  queue item appends a `->` state marker to the source item
  (devlog/README.md defines the forms). Commits and PR threads carry
  the what-changed.

<!-- /agents-md:managed:devlog -->

<!-- agents-md:managed:finish-line -->

## Default agent finish line

For any user request that asks you to change code, docs, assets, or project
state, the default endpoint is **an open, review-ready PR with required
checks green**, not a merged branch. Merging is a human decision; do not
merge your own PR unless the user explicitly asks, or the project has adopted
an opt-in self-merge workflow.

Use this checklist for each work session:

1. Read README plus the latest devlog entries, then start from `main`, or,
   for a follow-up that depends on an open PR, from that PR's branch (see
   Stacked PRs under Pull requests).
2. Create one correctly named branch for the work unit.
3. Make the scoped change, including docs/devlog/tests/assets that keep it
   complete.
4. Run the relevant verification plus the standard lint/build/test checks
   before PR; if any check cannot run, record the exact gap in the PR.
5. Commit one concern at a time with a body that says why.
6. Before opening a docs/chore PR (or at session end), grep the devlog
   for the open `## To promote` / deferred / needs-human queue and clear
   what the current scope covers, or explicitly re-defer, marking the
   source item; decided
   invariants shouldn't live only as devlog archaeology.
7. Push, open the PR with the template, and remove sections that do not apply.
8. Hand off per "Handing off the PR" (under Pull requests): start the
   review-watch, wait out required checks, handle reviewer activity,
   self-review the PR files view, and leave the PR open for a human to
   review and merge.

For changes on a **destructive path** (delete/cleanup), a
**credential-leak surface**, or a **returned-object-trust boundary**
(trusting fields of a value handed back by an external call or
deserializer), add a refute-first verification pass before committing
(independent lenses whose job is to _disprove_ the fix) and record in
the devlog which findings were confirmed, rejected-by-verification (so
they're not re-raised), and accepted-by-decision. For a
behavior-preserving refactor on one of these paths, where the platform
can execute code, have a lens reconstruct the
old implementation (`git show <base>:<file>`) and compare old against new
decision-for-decision over a fuzzed corpus; a diff-read can only assert
equivalence, a harness measures it. Scope all of this to those risk
classes; a docs typo or a refactor off these paths shouldn't trigger it.

<!-- /agents-md:managed:finish-line -->

<!-- agents-md:managed:context -->

## Context discipline

The working context is finite, and everything held in it is re-sent
with every later tool call, so transient bulk pulled in early taxes
every step after it. Durable state belongs in files (the devlog entry,
the PR body); keep the working context to what the current step needs.

- **Keep raw bulk out.** Prefer targeted, bounded reads and searches
  (a file region, a match list, a filtered log tail) over whole-file
  dumps and unfiltered search output; don't page a large artifact into
  context when a bounded query answers the question.
- **Delegate broad exploration.** Where your platform and session
  support delegation, offload broad exploration and mechanical sweeps
  to a delegate that returns conclusions (findings, `file:line`
  pointers, a short digest), never its raw output. Where they don't,
  fall back to the bounded reads and searches above. Scale to size
  either way: for a question a couple of targeted reads can answer,
  spawning a delegate costs more than it saves.
- **Right-size delegated work.** Where the platform exposes a model
  class or effort level for delegated work, send mechanical scanning
  and digesting to the cheapest class that handles it reliably;
  frontier capability spent on rote reading is waste. Where it
  doesn't, skip this.
- **No quiet fan-out.** One delegate for exploration or review is
  normal. Parallel multi-agent fan-outs multiply cost invisibly;
  before launching one, state the expected scale and proceed with the
  user's go-ahead or within a budget they already set.
- **Prefer a fresh session over a bloated one.** The devlog entry and
  the PR body carry the durable state, so at a natural boundary (a PR
  handed off, a review round closed, a new work unit) in a long
  session, suggest continuing in a fresh session seeded with the PR
  number and the entry rather than pushing on; the accumulated context
  adds little to the next unit and dominates its cost.

<!-- /agents-md:managed:context -->

## Build, test, run

The daemon (Wave 0 unit 1) and the API spec (Wave 0 unit 5) are initialized; the monorepo's other components are not. Per-component build, test, and run commands land in this table with each component's first PR (see `docs/plan.md` §11).

| Component     | Toolchain      | Commands                                      |
| ------------- | -------------- | --------------------------------------------- |
| `daemon/`     | Go             | `cd daemon`; `go build ./...`; `go test ./...`; `go vet ./...`; `golangci-lint run` |
| `app/`        | Xcode / SPM    | not yet initialized; see docs/plan.md roadmap |
| `api/`        | OpenAPI (spec) | `go run github.com/daveshanley/vacuum@v0.29.9 lint -r api/vacuum.ruleset.yaml --details --fail-severity warn api/openapi.yaml` (from repo root; see api/README.md) |
| `prompts/`    | prompt text    | not yet initialized; see docs/plan.md roadmap |
| `policy/`     | YAML (policy)  | not yet initialized; see docs/plan.md roadmap |
| `images/`     | OCI images     | not yet initialized; see docs/plan.md roadmap |

Lint/format and CI are established with the first component that carries code: the daemon does so here via `daemon/.golangci.yml` and `.github/workflows/daemon-ci.yml` (Linux runs build/test/vet/lint, macOS runs build/test). Later components add their own on the same pattern.

## Monorepo scope discipline

A work unit declares which component directories it touches, in the branch-name context and the PR body, and does not modify directories outside that declared scope. This is the manual precursor of Freeside's control-plane path restrictions (`docs/plan.md` §5.6, §5.8) and will later be enforced mechanically by the importer.

- Name the touched components in the PR body (a one-line "Scope:" is enough).
- Cross-component changes (typically `api/` plus both of its consumers, `daemon/` and `app/`) are **one work unit** and must say so; a spec change and its generated-code consumers move together, never in silently coupled separate PRs.
- Do not edit a component outside the current unit's declared scope to "fix while you're here"; file it instead.
- **A PR-time CI check (`.github/workflows/pr-integrity.yml`) enforces the part of this a diff can prove:** a change under a component dir (`daemon/ app/ api/ prompts/ policy/ images/`) whose component is not in the PR's `Scope:` fails, as does deleting or renaming a merged `devlog/` entry (frozen; append-only). It is not the importer (which guards the runtime candidate path, §5.6/§5.8) but the PR-time bridge to it, so it enforces the existing convention without redefining it; cross-cutting dirs (`devlog/` additions, `docs/`, `.github/`, root files) are never scope violations, and `repo-wide scaffold` opts out. Making it a required check is a repo-settings (maintainer) decision.

## Document gating

Changes to `docs/plan.md`, ADRs (`docs/decisions/`), and (later) the control-plane directories (`policy/`, `prompts/`) are reviewed like code, gated by **materiality** (`docs/plan.md` §9). Material changes — scope, acceptance criteria, milestones, sequencing affecting active work, architecture, risk posture, commitments — are **never batched silently into a feature PR**; wording and clarification changes are recorded in the PR that carries them, not separately gated.

- A material plan change is its own PR, unless the plan change *is* the direct subject of the feature PR (then it is called out explicitly in the PR body).
- ADRs are promoted from devlog entries (`docs/decisions/README.md`); the promotion is its own reviewed change.
- The materiality rules themselves are control-plane policy (plan §9); changing them is a material change.

## Automated reviewer

**Codex** reviews pull requests automatically. Respond to its findings per **Responding to automated review** under Pull requests, and filter later review activity by its login.

- **Login/account:** `chatgpt-codex-connector` (the `chatgpt-codex-connector[bot]` form appears on inline review comments and in the pulls review-comments API).
- **Triggered:** automatically on PR open-for-review, mark-ready, and each push (it re-reviewed after every push this session); also on demand via an `@codex review` comment.
- **Status signals:** on a **clean pass** (no findings) it posts no review and reacts 👍 (`+1`, i.e. `THUMBS_UP`) on the PR description a few minutes after the triggering event; that reaction, dated after the trigger, is the completion signal a review-watch keys off. On a **findings pass** it posts a `COMMENTED` review whose inline comments are each tagged by priority badge (P1/P2/P3) and invite a 👍/👎 reaction.

<!-- agents-md:managed:branches -->

## Branches

All work lands through a PR: branch from `main` (read `main` as the
repo's default branch throughout), do the work as atomic commits (see
Commits), open a PR; the work merges with a real merge commit, a
human's call per the finish line. Never commit directly to `main`. No
triviality exception: every bypass erodes the `--first-parent`
narrative.

Name branches `<type>/<short-kebab-slug>`: type from the Conventional
Commits vocabulary (`feat`, `fix`, `refactor`, `docs`, `chore`), slug
2–4 kebab-case words naming the work unit:

```text
feat/worksheet-promotion
fix/pane-focus-race
chore/swift-format-sweep
```

Exactly one slash: refs are path-like, so `feat/x` and a branch named
just `feat` can't coexist. No ticket numbers, dates, or owner prefixes;
prepend an owner segment (`bnw/feat/…`) only if multiple people or
agents start pushing in parallel. Merged branches auto-delete where
that repo setting is on (delete them after merge where it isn't); the
merge commit carries the narrative.

**Prefer a dedicated worktree per work unit.** Where your platform and
session support working from a second checkout (a native worktree tool
or session flag, or plain
`git worktree add <path> -b <type>/<slug> <base>`), do the work in a
dedicated worktree instead of the shared primary checkout, so parallel
agent sessions and the user's own work never collide on files, branch
state, or uncommitted changes. Remove the worktree once its branch
merges (`git worktree remove <path>`). Where they don't (no
multi-checkout support, or a sandbox pinned to one directory), fall
back to a branch in the primary checkout; the branch discipline above
still applies either way.

Follow-up work that depends on an open PR can stack on its branch instead
of waiting; see the Stacked PRs pattern under Pull requests.

<!-- /agents-md:managed:branches -->

<!-- agents-md:managed:pull-requests -->

## Pull requests

A PR is one work unit, reviewed as a whole and merged with a real merge
commit. Commits carry the atomic why (see Commits); the PR carries the
arc.

- **Title**: imperative, ≤ 72 chars, names the outcome, no type prefix
  or ticket noise ("Fix missing menu bar on unbundled launch"). In the
  intended repo setup the PR title (plus its number) is the _entire_
  merge commit message: merges are title-only, so the body's review
  material (screenshots, verification, review notes) never lands in
  history, and `git log --first-parent` reads as the list of PR
  titles; write the title for that log either way.
- **Body**: scaffolded by the repo's PR template (on GitHub:
  `.github/pull_request_template.md`):
  - **Why**: prose, one to three short sentences. State the problem or
    motivation. Link the devlog entry when one exists; don't duplicate it.
    Where the template's comment spells out issue keywords, follow it
    exactly: a close keyword per issue the PR fully resolves, a plain
    `Refs #N` for related-but-unfinished issues that are left for a
    human to close.
  - **What**: required bullets. Describe work-unit outcomes, not
    file-by-file churn. For multi-commit PRs, use a compact commit map
    (one bullet per commit or concern), referencing each commit by its
    subject, not its SHA: folding a review fix into its commit (see
    Commits) rewrites every downstream SHA, so a SHA-keyed map forces a
    body rewrite each round, while subjects don't go stale. Say rejected
    alternatives live in the devlog when they do.
  - **Screenshots**: required for PRs with visible UI changes; delete it
    for non-visual work. Replace the section with actual forge-hosted,
    reviewer-visible image or recording attachments before handing off,
    and in every case before merge; local paths, textual descriptions,
    and "checked locally" notes do not satisfy it. If you cannot attach
    the artifacts yourself, say so at handoff and ask the user to add
    or confirm them before merge. Show the changed surfaces,
    important states, and every theme or appearance mode the change
    affects. Keep captions short and name the state shown. Verification
    still belongs in Verification.
  - **Review Notes**: optional bullets; delete the section when it adds
    no routing value. Use it to point reviewers at important files, review
    order, mechanical commits, or risky edges.
  - **Verification**: required bullets. Start each with `Passed:`,
    `Checked:`, `Attempted:`, or `Not run:`. Say what was actually run and
    observed: tests, lint, fixture/screenshot checks (every affected theme
    for UI), round-trips for schema changes. Facts only, never
    "should work"; verification gaps are explicit `Not run:` bullets.
    Factual doc claims ship under the same discipline: counts, flags,
    behaviors, and runtime guarantees are checked against the code and
    scoped to the surface they describe, stated without marketing or
    competitor put-downs.
- **Self-review the diff in the PR files view before handing off**: seeing
  the whole change as one artifact catches stray hunks, leftover debug code,
  scope creep, and accidental files the editor hid. This is a
  _mechanical-hygiene_ pass; it does **not** substitute for substantive
  critique.
- **Substantive critique needs fresh, ideally non-self eyes.** Same-context
  self-review shares the blind spots that produced the code. Independence
  ladder, weakest to strongest: self-in-context < same-model fresh-context
  subagent < different-vendor bot / human. An automatic bot reviewer or a
  human is the load-bearing substantive pass; the default finish line
  already stops at an open PR for one.
- **Optional, risk-gated: a fresh-context pre-push review.** For non-trivial
  changes, or any repo without an external bot reviewer, get fresh eyes
  before pushing. **Where your platform and tools support delegation** (and
  it is allowed without asking), spawn a fresh-context reviewer: prompt it
  to _refute_, give it only the diff plus the PR's stated intent (not your
  reasoning trail), and let it hunt correctness, security, and edge-case
  failures. **Where they don't** (no subagent concept, or delegation needs
  explicit permission), skip it and lean on the external bot / human review,
  or ask the user first; never emit steps the running agent can't perform.
  A same-model subagent is only _partially_ independent and costs tokens;
  scale to risk, skip trivial or mechanical work.
- **Record a noticed automated reviewer.** When you observe a bot-authored
  review on a recent PR, or a reviewer status signal (a bot reacting on PR
  descriptions shortly after they open, recurring across PRs: a reviewer
  whose passes have all been clean may never post a review), and the project
  hasn't recorded the reviewer, add a compact
  record (an "Automated reviewer" entry; the required fields below usually
  take a short paragraph) to an unmanaged, project-specific section of
  AGENTS.md
  (outside `agents-md:managed:*` blocks, so syncs don't overwrite it) with
  enough identity to match its future reviews: the reviewer's **name**, its
  **login/account identity** (including the API-specific form when it
  differs, e.g. a `[bot]` suffix in one API but not another), how it is
  **triggered** (automatic on PR events, a manual command, or a CI job), and
  any **status signals** it posts out of band (an in-progress or clean-pass
  indicator, e.g. a reaction on the PR description; some reviewers post no
  review at all on a clean pass, so the recorded clean-pass signal is what
  lets a later watch finish instead of timing out). Later sessions filter
  review activity by that login, so the identity, not a bare "a reviewer
  exists", is the point. An existing record is not a reason to skip: when
  you observe status signals (or a changed trigger) the record lacks,
  augment it in place, since a name/login/trigger-only record still forces
  the full wait cap on clean passes. Record only a reviewer and signals you
  actually observed, never an absence.
- **Responding to automated review.** Evaluate each comment on its merits:
  fix real findings; push back, _with a one-line reason_, on contrived,
  speculative, or already-fixed ones; never reflexively comply. Reply
  inline with the disposition and the fixing commit SHA ("Fixed in
  `<sha>`" / a reasoned decline), then resolve the thread. Resolving every
  thread is _not_ a hard merge gate; evaluate-on-merits is.
- **Fix the class, not just the cited line.** When a finding names one
  location, sweep the file and repo mechanically (grep for the finding's
  pattern, don't just eyeball nearby lines) and fix every instance in the
  same push: the class routinely recurs in sibling sentences or files the
  citation never named, and each miss costs another review round. For
  validation or parsing code, the mechanical sweep is an adversarial
  enumeration of the input space (case, spacing, indentation,
  prefix/suffix, order, duplication, nesting), run once as tests, not a
  widening of the cited pattern: pattern-widening spent eight review
  rounds on one class before the enumeration closed it.
- **Converge deliberately, and don't under-converge.** Automated
  reviewers can surface ever-smaller nits indefinitely, so converge
  and hand off rather than chasing every round to zero (value captured
  is the bar, not threads-at-zero). But don't declare a PR "addressed"
  while the reviewer is still raising real issues, and never treat a
  finding that recurs from your _own_ incomplete fix as convergence;
  that is a miss to sweep, not a stop. Bias toward continuing while
  findings are genuinely worthwhile; the human's merge is the reliable
  convergence signal, not your own sense that you are done.
- **Keep the body current as review evolves the PR.** The body is the
  work unit's durable record on the forge (the merge commit carries only
  the title), so when review adds commits or shifts scope, update What,
  the commit map (flag which commits resolve review findings, by subject as
  above), and Verification before re-handing-off. The inline disposition +
  fixing SHA on each resolved thread (above) is the located per-finding
  record (that reply is written once, post-fold, so its SHA doesn't churn);
  don't duplicate it into a standing "feedback" section that would drift.
- The intended repo settings enforce the Commits rules: merge commits
  only (squash and rebase disabled), title-only merge messages, and
  auto-delete of merged branches. Don't re-enable around them; where
  they aren't set, hold the same rules manually (merge-commit merges
  only, the merge message kept to the PR title, delete the remote
  branch after merge).

### Handing off the PR

An open PR, not a merged one, is the agent's finish line; leave it
open for a human to review, approve, and merge, unless the user
explicitly asks you to merge or the project has adopted a self-merge
workflow. Done means open, green, threads handled, self-reviewed, and
no new review activity outstanding. Once the PR is up:

- **Start one review-watch per PR/reviewer as soon as the PR is open**,
  where the project records an automated reviewer or you have observed
  one, before waiting on checks, so the checks wait can't defer it.
  Prefer a dedicated review-watch skill, tool, or automation that can
  report back without manual polling; otherwise, if
  your platform can watch non-blockingly (a backgrounded poll or scheduled
  wake-up) and policy permits that mechanism, use it; don't pause to ask
  whether to watch. If a non-blocking mechanism would need permission not
  already granted, take the next permitted path. Where non-blocking support
  is absent, use a bounded foreground poll when it fits the current turn;
  otherwise hand back with the baseline and don't silently skip the review.
- **Anchor the watch baseline to the event that should produce the next
  reviewer pass**, not the moment the watch starts: the PR open/ready or
  actual push event for open/push-triggered reviews; the request time for a
  no-push recheck (marking ready, manually requesting review). Reviewer
  activity after that event is in-scope and must be handled, never absorbed
  into the baseline as already-seen. On a new push, advance or replace the
  baseline rather than leaving duplicate watchers running.
- **Wait for required checks**: poll them until they complete (on
  GitHub: `gh pr checks <n>`); fix any red check on the branch, never
  hand off a known-red PR.
- **Self-review the diff** (above) so it's ready for a reviewer.
- **Close out the watch before handoff**: poll for _both_ new review
  comments and CI, address in-scope findings on the branch, or record the
  bounded timeout / no-review result with the baseline; only then declare
  done.
- **Stop and summarize**: say the PR is open and green, and surface
  anything the reviewer should focus on. Leave merging, branch cleanup, and
  the `main` resync to whoever approves it.

If the user does ask you to merge, merge with a real merge commit (on
GitHub: `gh pr merge <n> --merge`; where the repo's title-only
merge-message settings aren't confirmed set, pass the message
explicitly instead of inheriting the forge default:
`gh pr merge <n> --merge --subject '<PR title> (#<n>)' --body ''`),
delete the remote branch if the
auto-delete setting didn't, then resync the base branch, delete the
local branch (`git branch -d <branch>`), and `git fetch --prune`. In a
single checkout the resync is `git checkout main && git pull --ff-only`;
when the work ran in a dedicated worktree (see Branches) `git checkout main`
refuses with "already used by worktree", so resync `main` in the primary
checkout and `git worktree remove <path>` the feature worktree before
deleting its branch.

### Reviewing a PR

The mirror of "Responding to automated review": hold the bar you'd want
held for you. Use the project's review tooling for the bug-hunting
pass where it has any, otherwise read the full diff yourself; these
are the conventions for the comments the pass produces.

- **Calibrate to severity, and tag it.** Separate blocking findings
  (correctness, security, data-loss, red tests/CI, broken invariants) from
  non-blocking ones (naming, style, optional simplification). Only blockers
  gate the merge. Don't manufacture speculative or contrived findings; the
  author convention is to decline those with a one-line reason.
- **Every comment carries evidence and a concrete ask.** Point at
  `file:line`, name the failure it causes, and propose a fix or ask a
  question. Mark uncertainty as uncertainty ("possible:"), never assert it;
  the Verification facts-only discipline applies to review too.
- **Review against intent, not just the diff.** Read the PR's Why/What and
  the devlog; check the change does what it claims, that Verification matches
  reality, and that docs/tests moved with behavior. Don't relitigate what the
  devlog marks decided or deferred.
- **Stay in scope.** Out-of-scope improvements are non-blocking nits or a
  follow-up issue, not merge-blockers; don't grow the PR through review.
- **Scale depth to risk.** Routine PRs get a normal pass; destructive /
  credential-leak / trust-boundary changes get the refute-first lens (see the
  finish line). A docs typo doesn't.
- **Resolve explicitly.** State what would unblock; let the author
  fix-or-decline. Resolving every thread isn't the gate; agreement on
  blockers is.

### Stacked PRs

Dependent docs or cleanup work can proceed without waiting for its base: a
follow-up PR can be based on an open PR's branch (on GitHub:
`gh pr create --base <feature-branch>`, which auto-retargets to `main`
when the base merges; on other forges retarget it manually). Two
gotchas: while the base is open the stacked PR's diff shows only its
own commits; and if the base is force-pushed (the fold-review-fixes
rule in Commits), `rebase --onto` the stack onto the new base tip.

<!-- /agents-md:managed:pull-requests -->

<!-- agents-md:managed:commits -->

## Commits

History is optimized for three uses: diagnostics (blame/bisect lead to a
cause), reviewability (a PR reads commit-by-commit), and learning (the
log tells the project's evolution). Rules:

- **One concern per commit, every commit green.** If the body wants
  labeled sections (Correctness:/Performance:/…), it's more than one
  commit; split it. Each commit must build and pass tests on its own;
  never leave red intermediate states (it breaks bisect).
- **Body says why, not just what.** Write dense, specific bodies,
  wrapped ≤ 72 columns. Reference the session's devlog entry
  when one exists. State change deltas ("27 → 36 tests") if meaningful;
  never absolute status ("36 tests green"); CI asserts that, and it
  goes stale.
- **Never commit secrets** (credentials, tokens, keys, `.env`
  contents); reference them by name and use placeholders in examples.
- **Mechanical churn commits alone.** Reformats, renames, and moves get
  their own commit, added to `.git-blame-ignore-revs` in the same change
  (activate locally with
  `git config blame.ignoreRevsFile .git-blame-ignore-revs`).
- **Fold review fixes into the commit they belong to.** When a review
  comment or self-review turns up a fix for code in an already-pushed
  commit, fold it into that commit rather than appending an "address
  review" commit; the merged PR keeps its clean, bisectable structure.
  Guardrails: every commit still builds and passes tests after the fold;
  `--force-with-lease`, **feature branch only, never force-push `main`**;
  only while the PR is unmerged (once merged, a fix is a new commit);
  update the matching devlog entry in the same operation. The mechanism
  (reset/amend/rebase) is your judgement.
- **Never squash-merge multi-commit work**: it destroys the atomic
  structure above. Merge with a real merge commit so
  `git log --first-parent` reads as the work-unit narrative and the full
  log holds the atoms. Narrative subjects ("Walking skeleton: end-to-end
  flow") belong at that merge/PR level.

<!-- /agents-md:managed:commits -->

<!-- agents-md:managed:done -->

## Definition of done for an increment

Each increment is something actively used by the end of the work session:
not "code complete" or "tests pass" alone, but running and exercised.
Before calling work done:

<!-- agents-md:project:done-checks -->

<!-- Pre-code scaffold: this repo holds no code yet, so the only
     verification is document coherence. Real per-component checks (Go
     test/vet/lint for daemon/, on Linux as well as macOS from day one
     per plan §3.3; swift build/test/format for app/; OpenAPI lint +
     generator round-trip for api/; schema validation for policy/) MUST
     be added to this block with each component's first PR, and the
     finish line's "lint/build/test" steps become live then. -->

- Docs coherent: README, AGENTS.md, and docs/plan.md do not contradict
  each other for the touched scope
- Scope declared: the PR body names which component directories the work
  unit touches (see Monorepo scope discipline)
- Devlog entry appended for the session

<!-- /agents-md:project:done-checks -->

<!-- /agents-md:managed:done -->

## Coordination

Coordination state lives in GitHub and git, never in status files. Issues
are the unit of work; this section defines how to find, claim, and finish
one. Runtime AttentionItems (docs/plan.md §4) are a different system; this
section governs building Freeside, not running it.

### Work units

One issue per work unit, created from the work-unit template: Source devlog
entry (the filename for a deferral escalation, `none` otherwise), Contract,
Acceptance (the fixture/test list is the spec), Declared paths, Dependencies.
Labels: `lane:*` for ownership area, `kind:*` for type. Milestones carry the
phase (1A, 1B). Each wave has a pinned tracking issue listing its units; the
spine role maintains it.

Here and below, **scheduled** means both a milestone and a listing on the
current tracking issue. The spine changes those fields as one planning
operation; either field alone is a spine-repair error and does not open the
scheduling door (fiat remains independent).

### Lane glossary (canonical)

Lane names are search keys and territory labels, defined canonically here;
subsystem-derived lane names (signet, gauntlet, publish is functional, ward)
also appear in docs/plan.md §15, which defines saddle and spine as
coordination vocabulary outside the subsystem register. They never appear in
code identifiers, package names, or API vocabulary, which stay functional
(the attention type is AttentionItem, not SignetItem).

| Lane | What it is | Owns (paths) | Plan |
|---|---|---|---|
| signet | Attention service: items, deliveries, conversations, sync, devices | daemon/internal/signet (api/ is shared contract territory: changes are `kind:contract`, drafted by the signet/saddle pair) | §4, §5.14 |
| gauntlet | Candidate path: export helper, hostile importer, clean verifier, evidence channel | daemon/internal/export, daemon/internal/importer, daemon/internal/verify | §5.6, §5.15 |
| publish | GitHub App auth, deterministic identities, reconciliation, EvidencePublisher | daemon/internal/publish | §5.5, §5.9, §5.11, §5.15 |
| ward | Runner backends, workspace-handoff gate, conformance, operating modes | daemon/internal/ward | §5.7 |
| saddle | SwiftUI clients (pipeline-exempt) | app/ | §5.14, §11 |
| spine | A ROLE, not a territory: serialized shared-contract changes (domain, migrations, interfaces, api/) and Wave 2 integration (workflow engine) | daemon/internal/domain, daemon/internal/store, daemon/internal/exec, daemon/internal/engine, daemon/migrations/, api/ | §11 |

### Claiming

Claim a unit by opening a **draft PR** that explicitly claims it
(branch per the Branches section) before substantive work. A fresh
branch has no PRable diff, so open the claim with an empty claim
commit (`git commit --allow-empty -m "Claim #N"`); the first work
commit follows on the same branch. Claim commits never merge: once
work commits exist, drop the empty commit in the next branch rewrite
(the fold-fix rules under Commits), before handoff at the latest; the
PR's close keyword carries the claim from then on. One claim per
unit; the active
claim is any open PR, draft or ready, that explicitly claims the unit
(a `Claim #N` commit or a close keyword for the issue), never one
that merely cross-references it (`Refs #N`): if an active claim
exists, pick another unit. Staleness applies only to claim-stage
drafts: a draft with no work commits in 48h may be superseded; note
the supersession in the old PR.

`needs-human` deferrals use the fiat door defined under Deferral escalation,
never self-selection: after the maintainer acts, fiat assigns the issue to a
session; the session verifies the external state, and its required devlog
entry supplies the audit diff for the ordinary close-keyword PR.

### Contract changes

Shared packages (domain types, migrations, StageDriver/ReviewSource/
RunnerBackend interfaces, the API schema) change only through
`kind:contract` units: exclusive, serialized, spine-owned, their own PR,
merged before dependents adapt. A contract PR carries its required
generated consumers and mechanical adapters (the cross-component
one-work-unit rule under Monorepo scope discipline); only downstream
feature work waits for the merge. Lane work never edits shared packages
in passing; needing a contract change means filing the contract issue,
linking it as a dependency, and blocking or switching units.

Before a `kind:contract` deferral is scheduled or assigned by fiat, the spine
inserts it into the serialized Dependencies chain; if it has no valid position,
it stays dormant. Fiat never bypasses contract ordering.

### Session start

1. Read docs/plan.md front-matter (revision, phase) and the sections your
   unit's Contract cites.
2. Read the latest devlog entries (Devlog section).
3. Status queries:
   - open PRs and their declared paths: overlap with yours means stop and
     coordinate via issue comment before claiming;
   - the current wave's pinned tracking issue;
   - open `kind:contract` issues, ignoring a `deferral` issue until it is
     scheduled or has an active claim, then excluding the unit you are claiming
     and any unit whose Dependencies chain includes it (a
     dependency-ordered chain of contract units keeps at most one
     claimable at a time, so downstream chain members may stay filed
     without blocking their chain head): among the remainder, if one
     touches your Contract, block on it; when claiming a
     `kind:contract` unit, block on every other remaining open contract unit
     (contract work is serialized).
4. Verify each dependency's PR is merged.

### Session end

Devlog bookends per the Devlog section. Additionally: deferrals discovered
mid-unit follow Deferral escalation below; tick your unit on the wave tracking
issue when your PR merges (or note partial state on the issue).

### Deferral escalation

Devlog queue items that escalate to issues (per the Devlog section: at
entry-writing time for items not draining within a session or two, or via
post-merge cleanup for items outliving their PR cycle) follow these rules:

- **Provenance both ways**: the issue form's required `Source devlog entry`
  field cites the source entry filename on its single line; the entry's item
  gets its `-> Refs #N` marker. Ordinary work units write `none` in the field.
- **Lane label routes by owner, not discoverer**: the lane whose Declared
  paths contain the work. Shared-package needs use `kind:contract` plus the
  **`deferral`** origin label.
- **For non-contract work, `kind:*` by the work's nature** (deferred scope:
  feature; known gap: fix; hygiene: chore), plus the **`deferral`** origin
  label.
- **Maintainer-only actions** (repo settings, credentials, App
  administration) get **`needs-human`** and no lane label. Self-selecting
  sessions and future scan initiators never pick up `needs-human` issues.
- **No milestone at escalation.** Open + `deferral` + no milestone is the
  unscheduled queue; the spine schedules eligible items during wave planning's
  deferral sweep and skips `needs-human`, which remains unmilestoned and
  fiat-only. Do not add status labels; milestone presence is the status.
- Closure is ordinary: a work-unit PR with a close keyword; the closed issue
  is the item's drain record per the Devlog section.

**Pickup: labels never authorize work.** An issue (deferral, adversarial
finding, or anything else except `needs-human`) becomes agent-actionable
through exactly two doors: **scheduling** (a spine sweep assigns its
milestone and lists it on the current tracking issue, from which sessions
self-select) or **fiat** (the human hands its number to a work-unit session,
which covers urgent items). A `needs-human` issue uses only fiat after the
maintainer acts, as Claiming defines. A session must never select work
directly by label or by browsing open issues. Sweep cadence: at every
planning session while waves exist; at phase boundaries after; ad hoc
whenever the human runs one. Between sweeps the unscheduled queue is dormant
by design; the Phase 1B scan initiator is the intended replacement for
human-cadence sweeping.
