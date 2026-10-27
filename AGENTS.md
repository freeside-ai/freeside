# AGENTS.md

**Freeside** is an agent control plane: a local, durable workflow controller that grants agents the autonomy to turn tasks into evidence-backed pull requests and interrupts a human only when judgment is required. The spec, architecture, and roadmap live in [`docs/plan.md`](docs/plan.md); read it first and argue changes against it. This file holds the conventions that apply to every session: decision notes, branch, PR, and commit discipline, and the monorepo's scope rules.

Freeside is a monorepo. A component directory holds only a `README.md` stating its purpose until the roadmap phase that fills it (`docs/plan.md` §11 and the README name the phase). Do not scaffold a component ahead of its phase. The API schema is provisional (plan §11 Wave 0; decision record in `docs/history/decisions.md`).

CLAUDE.md is a pointer that imports this file; edit AGENTS.md, never the pointer.

<!-- agents-md:managed:devlog -->

## Decision Notes (devlog)

`devlog/` holds selected decision records, not session logs. Most work needs
no note. In the ordinary case, keep at most one note per work unit or PR. Name
it `YYYY-MM-DD-HHMM-slug.md`. Follow the protocol in `devlog/README.md`.

- **Write a note only for a lasting decision or discovery.** A note is
  warranted when the work includes at least one of these:

  - A significant, non-obvious decision that rejects a reasonable option.
  - A finding that materially changes the model, policy, risk, or direction.
  - An owner decision that would otherwise exist only in chat.
  - Essential cross-session context that the issue or PR doesn't carry.
  - A change on the project's mandatory-note list, when it has one.

- **Skip notes for routine work.** Implementation, formatting, ordinary docs,
  dependency updates, mechanical syncs, large mechanical changes such as
  renames, and simple fixes need no note unless they reveal a lasting
  decision or discovery.
- **Find notes from the work first.** Read notes linked from the current issue
  or PR. Otherwise, search by path, topic, contract, or decision name. Read
  the latest note only when resuming the work unit it describes.
- **Treat old notes as evidence, not rules.** Do not silently overturn an
  explicit owner decision. When new evidence conflicts with one, name the old
  decision, explain which assumption or condition changed, and propose the
  revision.

<!-- /agents-md:managed:devlog -->

## Agent Setup

Agent-setup profile: High-assurance. A decision note is mandatory for:

- contract and safety-policy changes;
- material plan, architecture, or ADR decisions;
- destructive, credential-leak, or returned-object trust-boundary work;
- adversarial audits whose findings change policy or implementation;
- explicit owner choices that would otherwise exist only in chat.

Routine implementation and coordination need no note. GitHub issues and git
are the only sources of active work state; a note records why, never status.

### Coordination model

- **Current shape:** Shapes 2 and 3 on the landing baseline:
  path-and-dependency units with forge-visible claims, and typed relations
  for start order (`starts-after`), merge order (`merges-after`), and
  stacking (`stacked-on`), with no mutual exclusion. Authorized units start
  in isolated checkouts unless a recorded `starts-after` says otherwise, and
  the `required` check on the landing tip validates every merge
  (Coordination). No fixed width: open units are bounded by each wave
  tracker, live claims, and review bandwidth.
- **Evidence basis:** Waves 3–6 show recurring parallel fronts; PR #801
  established typed relations; Wave 6 tracker #835 identifies review bandwidth
  as the constraint. After Wave 8's seven-link chain serialized units with no
  recorded code dependence, owner decision #1709 (2026-10-02) limited contract
  serialization to assessed conflicts under a cap of two. The parallel-work
  proposal (freeasinbird/free-skills#291) found that start-time checks held
  back safe work yet let untested combinations reach `main`, and the owner
  adopted its rules on 2026-10-14
  (`devlog/2026-10-26-1505-start-freely-validate-at-landing.md`).
- **Detailed mechanics:** [`docs/coordination.md`](docs/coordination.md).
- **Reassess when:** After the first wave under these rules, read the four
  measures (how often a PR fails at landing and why, how often a rebase
  conflicts and in which files, how long a PR waits to land, and how much
  work is discarded because a dependency was missed) and reassess when any
  of them shows the rules costing more than the serialization they replaced.

## Work-Unit Stages

Freeside's work-unit stages are planning and implementation. Review
convergence, the human merge gate, and post-merge cleanup stay phases of the
finish-line workflow, not stages. A stage adds no authorization door and
weakens none of the claim, relationship, review, or merge gates in this file.

### Planning

- **Activation:** Explicit owner fiat in the form `Plan #N`. A bare issue,
  label, claim, existing plan, or satisfied dependency does not activate it.
- **Allowed mutations:** Only these, and only under a planning reservation
  that is unexpired with enough margin left for the write and its post-write
  verification (`docs/coordination.md` §Stages): the assigned issue's body,
  as the authoritative work contract; the issue's one versioned
  planning-reservation comment, replaced in place by the current
  implementation-plan comment or an explicit release marker; an edit or
  explicit non-current marker on a superseded plan comment; and trackers
  whose projections derive from an edited Dependencies field. Expiry ends
  planning-write authority; continue only with a fresh reservation obtained
  through that document's recovery procedure. A mutation whose verification
  finishes after expiry stays visible but unverified and never satisfies the
  planning finish line; the only write allowed then is the recovery-only
  partial-state report that procedure requires, which is neither planning
  output nor authority to continue. While active, the reservation blocks
  implementation of its own issue and no other unit; it is not a claim or an
  authorization door. No claim,
  branch, PR, code, or implementation change is allowed.
- **Required input:** The assigned issue and freshly resolved default-branch
  state; when changing Dependencies, the complete containing-tracker discovery
  and current projection inputs that `docs/coordination.md` requires. Apply
  Forge Edits below; an enforced writer lock or atomic transaction is not a
  planning precondition.
- **Durable output:** The completed issue-body contract and one current
  implementation-plan comment.
- **Finish line:** Both outputs verified on the forge, with no claim or
  implementation started.
- **Transition:** Owner fiat hands the unit to implementation with `Handle #N,
  implementation plan in comments`; an already scheduled issue may instead
  enter implementation through the existing pickup door. A completed plan
  never authorizes implementation by itself.

### Implementation

- **Activation:** Explicit owner fiat in the form `Handle #N` for an
  issue-backed unit, a direct session-contained owner assignment with no
  issue, or pickup through the project's explicitly authorized scheduling
  door. When planning
  ran first, the input includes the plan in comments.
- **Allowed mutations:** The ordinary work-unit surface: its claim, isolated
  branch or worktree, declared paths, implementation and verification
  artifacts, review responses, and PR.
- **Required input:** The authoritative issue contract or, for a direct
  session-contained assignment, its prompt-backed work contract; when a
  planning stage ran, its single current implementation-plan comment. Execute
  that plan instead of replanning unless it conflicts with the work contract,
  this project's policy, dependencies, or current code reality; then the
  authoritative source wins and you surface the mismatch.
- **Durable output:** An open, evidence-backed PR carrying the implementation
  and its verification record.
- **Finish line:** The Default Agent Finish Line in this file: an open,
  review-ready PR with required checks green.
- **Transition:** A human decides whether to merge; after a verified merge,
  post-merge cleanup follows the record below.

## Merge Cleanup

### Post-merge obligations

- **Containing trackers:** For an issue-backed merged unit, find every open
  tracker (wave, feature, or backlog) that lists its verified closing issue.
  A closed tracker, including a completed wave's, is never mutated. Zero open
  containing trackers is a valid zero-work result, not an
  incomplete-reconciliation error. A direct, session-contained unit has no
  containing tracker.
- **Refresh:** In each containing tracker, as one edit, apply the refresh in
  `docs/tracker-format.md` (§refresh) with Freeside's **Startable now** rule
  from `docs/coordination.md` (Tracking Issues). The tracker carries start
  order only; there is no **Mergeable next** to recompute.
- **Detailed mechanics:** `docs/coordination.md`.
- **Report:** Name newly unblocked units without claiming or starting them.
  The required check reruns on each open PR when it is next brought up to
  date, so the report names no invalidated integration evidence.

<!-- agents-md:managed:finish-line -->

## Default Agent Finish Line

For changes to code, docs, assets, or project state, finish with an open,
review-ready PR and green required checks. Leave the PR unmerged. Merge only
when the user asks or the project has an explicit self-merge policy.

Before implementation, define a small work contract:

- Objective.
- Testable acceptance criteria.
- Scope.
- Dependencies and blockers.
- Explicit non-goals.

A direct user request needs no issue. The request and PR carry its contract.
Use a tracker issue when the work must:

- Continue in a later session.
- Pass between agents or sessions, even during one short session.
- Coordinate concurrent workers.
- Enter a backlog.

When one agent or session hands work to another, use the issue and its
comments. Put there what the next one needs and what the previous one produced,
not only chat. Before handoff, create an issue for actionable work deferred
beyond the current scope.

A project may define optional work-unit stages in a project-specific section
outside the managed blocks. An active stage controls what may change and where
to stop:

- An implementation stage runs only its allowed checklist steps and stops
  where the active stage says to stop.
- A non-implementation stage follows its own record.
- Finishing one stage hands work off. It doesn't authorize the next stage.
- Work outside a declared stage runs the full checklist, except actions owned
  by another declared stage.

Start work only from an explicit user assignment. An issue, label, backlog
entry, satisfied dependency, completed plan, or claim isn't authorization.
An agent may choose work for itself only when an explicit project policy
allows it.

The implementation checklist:

1. Read the README. When resuming work, also read its issue or PR and linked
   decision notes. Resolve the default branch and update it from its remote.
   Start from that exact tip. Only a declared stacked PR may start elsewhere;
   see Branches.
2. Create a correctly named branch in a dedicated worktree or equivalent
   isolated checkout. See Branches for the primary-checkout exception.
3. Make the scoped change. For a visible UI change, capture the current state
   before you change it. Include the docs, tests, and assets needed to keep
   the change complete. Add a decision note only when its triggers apply.
4. Run relevant verification and the standard lint, build, and test checks.
   Record any check you could not run in the PR.
5. Commit one concern at a time. Explain why in each commit body.
6. Push and open the PR with the template. Remove sections that don't apply.
7. Follow "Handing Off the PR" under Pull Requests. Leave the PR open for a
   human to review and merge.

Before committing work on a destructive path, credential-leak surface, or
returned-object trust boundary, read `docs/agent-workflow.md` §refute-first and
run its verification pass. A destructive path includes delete or cleanup. A
returned-object trust boundary is where code trusts fields returned by an
external call or deserializer. This extra pass doesn't apply to a docs typo or
unrelated refactor.

<!-- /agents-md:managed:finish-line -->

<!-- agents-md:managed:context -->

## Context Discipline

Working context is limited. Content added now is sent again with later tool
calls, so early noise makes every later step more expensive. Keep durable
state in files, such as the issue, PR body, or decision note. Keep only what
the current step needs in working context.

- **Keep raw bulk out.** Prefer a relevant file section, match list, or
  filtered log tail over a whole file or unfiltered output.
- **Delegate broad reading when supported.** Use a delegate for large searches
  or mechanical sweeps only when the platform and session permit it. Ask for
  conclusions, `file:line` references, and a short summary, never raw output.
- **Use bounded reads when delegation is unavailable.** A few targeted reads
  are also better than a delegate for a small question.
- **Match the delegate to the task.** When you can choose a model or effort
  level, use the cheapest capable option for mechanical reading. Skip this when
  the platform offers neither choice.
- **Explain large parallel work first.** One delegate for exploration or
  review is normal. Before using more, state the expected scale and get the
  user's approval or stay within a budget they already set.
- **Suggest a fresh session at a natural boundary.** After a PR handoff,
  review round, or work unit, a long session adds little value. Suggest a new
  session seeded with the PR number. The PR and decision note carry the state.

<!-- /agents-md:managed:context -->

<!-- agents-md:managed:communication -->

## Writing for Humans

People scan human-facing work such as handoffs, PRs, issues, plans, reviews,
and questions. Make the important point clear without requiring them to
translate agent jargon or search for the conclusion.

- **Lead with the bottom line.** Start with the conclusion, decision, or ask.
  Include any assumption or caveat that could change it. Put support below in
  order of importance.
- **Front-load each unit.** Begin every heading, bullet, and paragraph with
  its key words.
- **Layer detail.** Keep the decision in the skim layer. Put evidence,
  options, and detail below it or in a linked issue or note. Do not remove
  needed evidence just to make the text shorter.
- **Ask about three questions per round.** Start with questions that block the
  work. Give each a recommended answer and one-line reason. Turn questions
  with a safe default into visible assumptions the reader can reject. Save
  remaining blocking questions for the next round.
- **Reserve flags for meaningful risk.** Label severity when useful. Flag
  facts that change the decision or confidence in the result. Make rare,
  critical warnings easy to notice.
- **State uncertainty plainly.** Say what was not verified and what remains
  uncertain. Clear writing must not make weak evidence look conclusive.

<!-- /agents-md:managed:communication -->

## Build, Test, Run

Run standard checks from the repo root with
`bash scripts/check.sh <component> [step...]`; `--list` lists components and
steps. Its step table is authoritative. Read its header for tool overrides
and opt-in live suites. CI uses the same script except daemon lint, which
uses golangci-lint-action at the same pinned version.
`.github/workflows/pull-request.yml` has no path filter: it calls reusable
workflows for changed components and reports `required`. Branch protection
requires that check and an up-to-date branch. Leave a PR behind `main`, before
handoff and after it: the lander (`land-approved-prs`) refreshes it when it
selects the PR. Refresh it onto `main` yourself only when the merge-result
audit finds a conflict, a landed change needs a fix in it (including a
contract PR's regenerated consumers, Contract Changes), the lander hands it
back (`needs-human`), or the owner asks (`docs/agent-workflow.md`
§handing-off step 3).

Operator
tools that are not checks live with their component: installers in
app/README.md, image builds in images/README.md, the real unattended run in
daemon/README.md (Run Real Work With The Deputy), and the API linter pin in
api/README.md. `prompts/` and `policy/` are not yet initialized. Lint,
format, and CI follow the daemon's pattern, `daemon/.golangci.yml` and
`.github/workflows/daemon-ci.yml` (Linux runs build, test, vet, and lint;
macOS runs build and test); each new component adds its own on that pattern
in its first PR, registers its steps in `scripts/check.sh`, and registers
its paths in `pull-request.yml`.

Before inspecting daemon state or waiting for a change, read
`daemon/README.md` (Read A Task's Record), including its stopped-root recipe
requirements. For a running daemon, use `freesided inspect` instead of
database queries or logs.
Start a development daemon with `bash scripts/dev-instance.sh --daemon-only`;
it prints `root=<root>`, with the database at `<root>/daemon/freeside.db`.

While iterating on a visible `app/` change, render only the surfaces the step
touches with `bash app/scripts/render-surfaces.sh OUTPUT_DIR SURFACE...` and
run only the affected test suites. At the end, record screenshot digests per
render-changing commit, then run the full `app` test step once before push.
The full screenshot matrix takes minutes; a selected render takes seconds.
Detail: `app/README.md` (Iterating on a Visible Change).

## Daemon Coding Conventions

Binding for new and changed `daemon/` Go code; the detail lives at point of
use, not here. The rules are a ratchet, not a retroactive claim: a deviation
that predates these conventions gets a tracker issue and drains as its own
unit, never a fix in passing (Monorepo Scope Discipline).

- **Enums:** a named string type with a `valid()` predicate and an `AllX`
  slice as the single registration point; the zero value `""` is invalid by
  design. Detail: `daemon/internal/domain/doc.go`.
- **Switches over enums:** a validity `valid()` switch uses `default` because
  it is a predicate. A switch that dispatches behaviour omits `default`, so
  the `exhaustive` linter (`default-signifies-exhaustive: true` in
  `daemon/.golangci.yml`) forces a new member to be handled, with a trailing
  fallback return for the invalid zero value.
- **Golden tests:** `json.MarshalIndent` of a fixed, valid fixture: UTC-fixed
  times, pointer-for-optional rendering explicit null, and no map fields in
  the contract shapes goldens pin (a package-private persistence format is
  not one). Fixtures double as validation-positive cases. Worked example:
  `daemon/README.md`; shared helper: `daemon/internal/golden`.
- **Trust boundaries at reconstruction/persistence:** a boundary that decodes
  a row or accepts an exported struct re-runs the trusted policy gate against
  current state (for example the approved-recipe set). A decoded or
  caller-supplied trust bit (`publish_eligible`, recipe approval, a
  provenance head) is never trusted, and the re-gate fails closed. Detail:
  `daemon/internal/store/entities.go`, `daemon/internal/domain/artifact.go`.
- **Timer-dependent tests:** a test whose behavior depends on real stdlib
  time in the code under test (`time.Timer`, `time.Ticker`, `time.After`, a
  `context` deadline) runs inside a `testing/synctest` bubble, not a
  real-clock sleep or poll. This is a ratchet on new or substantially revised
  tests, not a retrofit sweep, and applies only where the code uses the real
  `time` package; injected-clock behavior (the scheduler's occurrence-due
  logic, the janitor and engine `now`) is already deterministic. Detail:
  `daemon/README.md`; worked example:
  `daemon/internal/scheduler/run_synctest_test.go`.
- **Executed test fixtures:** a test that writes a file it then executes,
  directly or through `PATH`, writes it with `execfixture.Write`, so a
  parallel fork can't make the exec fail with `text file busy`. Fixtures
  nothing executes stay as they are; this is a ratchet, not a sweep.
  Detail: `daemon/internal/execfixture/execfixture.go`.
- **Fixture git environments:** a test helper that runs git starting from
  the process environment builds it from `gitenv.Scrubbed()` and appends its
  pins, so an inherited `GIT_DIR` never retargets it at the developer's
  repository. `scripts/check.sh daemon test` fails when a helper that skips
  it writes through an inherited `GIT_DIR`. Detail:
  `daemon/internal/gitenv/gitenv.go`.

## Monorepo Scope Discipline

A work unit declares which component directories it touches, in the
branch-name context and the PR body, and changes nothing outside that scope.
This is the manual precursor of Freeside's control-plane path restrictions
(`docs/plan.md` §5.6, §5.8), which the importer will later enforce
mechanically.

- Name the touched components in the PR body; a one-line "Scope:" is enough.
- A cross-component change (typically `api/` plus its consumers `daemon/` and
  `app/`) is **one work unit** and must say so. A spec change and its
  generated-code consumers move together, never in silently coupled separate
  PRs.
- Do not edit a component outside the declared scope to fix something in
  passing; file an issue instead.

## Document Gating

Changes to `docs/plan.md`, ADRs (`docs/decisions/`), and later the
control-plane directories (`policy/`, `prompts/`) are reviewed like code and
gated by **materiality** (`docs/plan.md` §9). A material change (scope,
acceptance criteria, milestones, sequencing that affects active work,
architecture, risk posture, commitments) is never batched silently into a
feature PR. A wording or clarification change is recorded in the PR that
carries it, not gated separately.

- A material plan change is its own PR, unless the plan change is the direct
  subject of the feature PR; then the PR body calls it out explicitly.
- An ADR is promoted from a decision note (`docs/decisions/README.md`); the
  promotion is its own reviewed change.
- The materiality rules are themselves control-plane policy (plan §9);
  changing them is a material change.

## Markdown Conventions

Use **title case** at every heading level in new docs and headings touched by
substantial revisions (`docs/intro.md` is the example). Keep existing title
case; leave other headings alone until a substantive revision touches them.
A heading-only retitle is not a work unit.

Keep machine-read record headings exact, including
`### Post-merge obligations` and `### Coordination model`.

## Forge Record

- **Use `freeside-ai/freeside` on GitHub for forge calls.** This project has
  the following forge record:

  - **Host:** `github.com`.
  - **Slug:** `freeside-ai/freeside`. Pass `--repo freeside-ai/freeside` to
    `gh`; never derive the owner from a sibling project.
  - **Consumers:** `await-pr-review`, `land-approved-prs`, `merge-cleanup`,
    and `visual-evidence` read this record before inferring a repository
    from a remote.

## Automated Reviewer

**None.** The owner turned off automatic Codex review on 2026-10-21, so no
bot reviews pull requests.

- **Reviewer:** `none`, an owner decision. Only the owner changes it.
- **Review watch:** invoke `await-pr-review` as step 1 of Handing Off the
  PR. It reads the `none` record above, starts no review watch, scans no
  past PRs, and waits on the PR's CI checks only. Keep the record here: the
  skill doesn't treat the landing record's `Reviewer: none` as one.
- **Before pushing:** follow `docs/agent-workflow.md` §pre-push-review, which
  asks for a fresh-context reviewer when delegation is available and
  otherwise relies on the owner. The owner's review is the final one.
- **No new record from stray activity:** an old summary comment or an
  on-demand `@codex review` is not a reviewer to record; a new record needs
  the owner to turn automatic review back on.

Decision records: `devlog/2026-10-21-0848-retire-codex-reviewer.md` and
`devlog/2026-10-21-1420-await-review-ci-only.md`.

## Landing Record

- **Land approved PRs by label.** This project has the following landing
  record:

  - **Mechanism:** `label`.
  - **Approval:** `approved`, added by a collaborator with the Approver
    permission; agents never add it.
  - **Hand-back:** `needs-human`.
  - **Approver permission:** `write`.
  - **Stale lock age:** `240` minutes.
  - **Base-advance checks:** none.
  - **Reviewer:** `none`.
  - **Consumer:** `land-approved-prs`.

The record is read from `main`'s tip, so a change to it takes effect only
once it merges. Base-advance checks stay empty because the required check
already reruns on the branch updated to the tip, and the lander waits for it
on that exact commit. The merge-result audit needs each PR's declared scope,
so the lander runs it per PR (Integration Ordering and Merge-Result Audit);
a fixed command here cannot. Reviewer is `none` because the project has no
automated reviewer (Automated Reviewer). Decision records:
`devlog/2026-10-19-1500-landing-record.md`,
`devlog/2026-10-19-1903-permission-based-approval.md`, and
`devlog/2026-10-21-0848-retire-codex-reviewer.md`.

## Freeside Review-Loop Bound

The managed review-convergence guidance runs under Freeside's resolved policy:
round counts are emergency brakes, not the normal stopping rule, but policy
exhaustion is still a mandatory stop. When the bound is exhausted or the
result is ambiguous, stop and create a durable AttentionItem. Thrash is an
additional stop condition, not a replacement for policy exhaustion or
escalation.

## Integration Ordering and Merge-Result Audit

Rule 2 (Coordination) carries integration: branch protection's up-to-date
rule makes a PR's branch contain the tip it lands on, and the `required`
check passes on that commit, so the check reruns on every refresh and nobody
revalidates a waiting PR after each base advance. A refresh the author pushes
still gets the review of the refreshed diff in `docs/agent-workflow.md`
§handing-off step 3, and the audit below. The merge-result audit's script
header explains its purpose and guarantees.

- **The owner orders landing.** `land-approved-prs` lands a batch in
  approval order or in the order the owner types (`--order`), and reads no
  typed relation. A `merges-after` relation is therefore enforced only by
  the order the owner approves PRs or types to the landing batch. The
  planner tells the owner at handoff when a unit needs one, and a unit's
  handoff report names any `merges-after` prerequisite that has not merged.
  Record `stacked-on` bases in each unit's Dependencies field (Stacked PRs).
- **Refresh by rebase only.** Refresh a PR branch, including a stacked child,
  by rebasing onto its freshly fetched base, then pushing with
  `--force-with-lease`. Never merge the base into a PR branch, locally,
  through the forge, or through a host tool (`docs/agent-workflow.md`
  §handing-off). Resolve each conflict in the commit that owns the change
  and recheck that every rebased commit is green. Refresh before handoff
  only for a conflict or a landed change that needs a fix in this PR, such
  as another contract unit whose change this contract PR's generated
  consumers must be regenerated against (Contract Changes). Decision record:
  `devlog/2026-10-06-1236-rebase-base-refresh.md`.
- **Read a conflicted rebase (rule 3).** After a rebase that conflicted,
  read what the resolution changed with `git range-diff` and leave the
  rebase record. The record has two forms: the author's, the range-diff
  reading in the PR's Verification section after a rebase the author ran;
  and the lander's, the PR comment with the range-diff that a
  `land-approved-prs` batch posts when it rebased or reworked the PR beyond
  a clean rebase (that skill's Comments and the Batch Report).
  `scripts/landing-measures.py` counts both, within the limits its header
  states. Detail: `docs/coordination.md`
  (Reading a Rebase).
- **Run the audit before handoff and after each rebase.** Fetch the default
  branch and run
  `scripts/merge-result-audit.sh origin/main <head-branch> <allowed-path>...`
  against that exact tip, allowing only the unit's declared paths. Read the
  whole printed change set; an in-scope reversion still passes the audit.
  Name that tip's SHA in the handoff report's Status line
  (`docs/agent-workflow.md` §handing-off step 7), not the PR body. A
  passing audit needs no other record; a failing one means the PR is not
  ready to merge. A clean rebase can change the paths a branch touches by
  following an upstream rename, so until the audit's scope check runs in
  the `required` check, it runs after every rebase: the author runs it
  after a rebase the author ran, and the landing batch runs it per PR by
  hand after each rebase it makes, because the Landing Record names no
  base-advance checks (`devlog/2026-10-19-1500-landing-record.md`).

<!-- agents-md:managed:branches -->

## Branches

All work lands through a PR. Resolve the default branch (`main` in the
examples) and update it from its remote. Then create an ordinary work-unit
branch from that exact tip. Never start from the current feature branch. Only
a declared stacked PR may use another base.

Use atomic commits and a real merge commit. Let a human decide when to merge.
Never commit directly to `main`, even for a small change. Direct commits break
the `--first-parent` history.

Name a branch `<type>/<short-kebab-slug>`:

- Choose a Conventional Commits type: `feat`, `fix`, `refactor`, `docs`, or
  `chore`.
- Use two to four kebab-case words for the work unit.
- Use exactly one slash. A bare `feat` can't coexist with `feat/x`.
- Omit ticket numbers, dates, and owner prefixes.
- Add an owner segment, such as `bnw/feat/...`, only when several people or
  agents work in parallel.

Examples:

```text
feat/worksheet-promotion
fix/pane-focus-race
chore/swift-format-sweep
```

Merged branches may auto-delete. If the repository doesn't do that, delete
the branch after merge.

**Plan concurrency before creating worktrees.** Keep coupled work in one work
unit, an explicit dependency chain, or a declared stack. Separate worktrees do
not make dependent changes safe to run in parallel. Before substantive work,
use the project's claim visible on the code host for an assigned concurrent
unit, when one exists. A claim only tells others that someone is already
working; it isn't permission to start.

**Isolate every implementation work unit.** Use a dedicated worktree or an
equivalent separate checkout when the platform and session support one. Create
it from the freshly updated default-branch tip. For example:

```sh
git worktree add <path> -b <type>/<slug> <default-branch>
```

Use the primary checkout only when the user or project requires it, or the
platform can't create another checkout. This can happen with no multi-checkout
support or a sandbox pinned to one directory. In that case, serialize work on
one correctly based branch, report the exception, and never run concurrent work
units in that checkout.

After merge, remove the worktree while standing outside it:
`git worktree remove <path>`.

Work that depends on an open PR may stack on its branch. See Stacked PRs under
Pull Requests.

<!-- /agents-md:managed:branches -->

<!-- agents-md:managed:pull-requests -->

## Pull Requests

One PR represents one work unit. Review it as a whole and merge it with a real
merge commit. Commits explain each atomic decision; the PR explains the full
change.

- **Write an imperative title of at most 72 characters.** Name the outcome,
  without a type prefix, ticket number, or other tracking text. The title and
  PR number become the whole merge-commit message in the intended setup. Write
  it for `git log --first-parent`.
- **Use the PR template for the body.** Include Why, Scope, What,
  Departures, Screenshots for UI changes, optional Review Notes, and
  Verification. Departures is required; write "None" when nothing departs.
  Key any commit map by subject, not SHA. Start verification bullets with
  `Passed:`, `Checked:`, `Attempted:`, or `Not run:`. Before writing or
  updating the body, read `docs/agent-workflow.md` §pr-body. For a UI change,
  meet its Screenshots requirements.
- **Self-review the full diff in the PR files view.** Look for stray changes,
  debug code, scope creep, and accidental files. This catches accidental
  changes; it doesn't check whether the solution is correct.
- **Repeat integration checks when the base moves.** Before merge, follow
  `docs/agent-workflow.md` §handing-off step 3. A landing check reruns CI,
  not review; a refresh you push restarts the review watch.
- **Use fresh eyes for substantive review.** Reviewing your own work in the
  same conversation shares the author's blind spots. A review in a fresh
  conversation is more independent. A bot from another provider or a human is
  stronger. Rely on a bot or human before handoff. Before pushing risky work,
  or any work without a bot reviewer, read `docs/agent-workflow.md`
  §pre-push-review. Risk means a destructive path, a credential-leak
  surface, a returned-object trust boundary, a contract change, or untested
  behavior.
- **Record an automated reviewer you observe.** If the project has no record
  for that reviewer or signal, and its reviewer record isn't `none`, read
  `docs/agent-workflow.md` §reviewer-record and update the project record
  before handoff.
- **Judge review comments on their merits.** Fix real findings. Decline
  speculative, contrived, or already-fixed findings with a one-line reason.
  Do not comply automatically.
- **Reply after the fix is final and pushed.** Reply inline with the outcome:
  the final commit SHA for a fix, or the reason for a decline. Then resolve the
  thread. Fold all fixes from one round into their owning commits and push once
  before replying. Resolving every thread isn't a merge gate; a reasoned
  outcome is.
- **Fix the whole defect class.** Search the file and repository for the same
  pattern and fix every instance in one push. For validation or parsing code,
  read `docs/agent-workflow.md` §review-convergence before widening a pattern.
- **Keep reviewing while blockers remain.** Correctness, security, data loss,
  broken invariants, and red CI always require another round. Decide severity
  yourself; the reviewer's label is only evidence. When a reachable defect's
  severity is unsure, treat it as blocking. When its reachability is unsure,
  trace the callers or run the case before patching.
- **Raise the bar as rounds continue.** After the early rounds, when a finding
  recurs, or when findings cite code an earlier round added, read
  `docs/agent-workflow.md` §review-convergence before deciding on another
  round. Before handoff, mark every finding fixed, declined, deferred, or
  explicitly outstanding.
- **Keep the PR body current.** When review adds commits or changes scope,
  update Scope, What, Departures, and Verification, and any commit map. Keep
  each finding's outcome in its inline reply, not a permanent feedback
  section. Once images are hosted, start a later body edit from the live body.
- **Keep the intended repository rules.** Use merge commits only, disable
  squash and rebase merges, use title-only merge messages, and auto-delete
  merged branches. Do not re-enable a disabled method. Enforce these rules
  manually where repository settings don't.

### Handing Off the PR

A PR is ready to hand off when it's open, green, self-reviewed, has no
unhandled threads, and has no outstanding review activity. After opening the
PR, read `docs/agent-workflow.md` §handing-off and follow its sequence.
End with the report its step 7 defines.

Once the approver approves a PR to land, stop pushing to it unless you are the
landing batch. Turn off any auto-fix, CI repair, or review watch the platform
runs for it, where the platform offers that control; otherwise tell the
approver which automation still runs.

If the user asks you to merge, read
`docs/agent-workflow.md` §merge-and-resync first and follow it step by step.
Do not merge or resync from memory.

### Reviewing a PR

Before reviewing a PR, read `docs/agent-workflow.md` §reviewing-a-pr and use
its review bar.

### Stacked PRs

Before creating a branch or PR that depends on another open PR, read
`docs/agent-workflow.md` §stacked-prs. Name the base explicitly; never inherit
it from the current checkout.

<!-- /agents-md:managed:pull-requests -->

<!-- agents-md:managed:commits -->

## Commits

History supports diagnosis, review, and learning. Keep each commit useful for
all three.

- **Keep one concern in each commit, and keep every commit green.** Split a
  commit whose body needs separate labels such as Correctness and Performance.
  Each commit must build and pass tests on its own. Never leave a red
  intermediate state that breaks `git bisect`.
- **Explain why in the body.** Use specific body text wrapped at 72
  characters. Link the work unit's decision note when one exists. Report a
  meaningful change as a delta, such as "27 to 36 tests", not an absolute
  claim such as "36 tests green" that will go stale.
- **Never commit secrets.** Keep credentials, tokens, keys, and `.env` values
  out of commits. Name the secret and use a placeholder in examples.
- **Separate mechanical churn.** Put formatting, renames, and moves in their
  own commit. Add that commit to `.git-blame-ignore-revs` in the same change,
  then enable it locally with
  `git config blame.ignoreRevsFile .git-blame-ignore-revs`.
- **Fold review fixes into the commit that caused them.** This includes issues
  found by review or self-review. Do not append an "address review" commit.
- **Keep every folded commit green.** Fold only on an unmerged feature branch.
  After merge, use a new commit. Update the matching active decision note in
  the same operation when one exists.
- **Force-push safely after a fold.** Use `--force-with-lease` on the feature
  branch. Never force-push `main`. The reset, amend, or rebase mechanism is
  your choice.
- **Push before replying to review.** The inline reply must cite the final,
  pushed SHA that contains the fix. A separate review-fix commit left on the
  branch means the fold is unfinished.
- **Never squash-merge multi-commit work.** Use a real merge commit so
  `git log --first-parent` shows the work-unit story and the full log preserves
  its atomic commits. Put narrative subjects such as "Walking skeleton:
  end-to-end flow" at the merge or PR level.

<!-- /agents-md:managed:commits -->

## Mechanical Commit-Message Checks

Before the first commit in a clone, run `git config core.hooksPath .githooks`.
The commit-message hook is best-effort; range-mode CI over recorded commits
is authoritative. Before committing, read `scripts/check-commit-messages.sh`'s
header for the rules and hook limitations. Check a branch locally with
`bash scripts/check-commit-messages.sh origin/main HEAD`.

The same opt-in enables the `pre-commit` hook that regenerates the tracked
API client when any of its inputs is staged; CI's `generate` job remains the
backstop.

<!-- agents-md:managed:done -->

## Definition of Done for an Increment

An increment is done only when it's running and exercised by the end of the
work session. "Code complete" or passing tests alone isn't enough.

Before calling the work done, confirm that the build succeeds, tests pass,
and lint and formatting are clean.

<!-- agents-md:project:done-checks -->

- Docs coherent: README, AGENTS.md, and docs/plan.md do not contradict
  each other for the touched scope
- Scope declared: the PR body names which component directories the work
  unit touches (see Monorepo scope discipline)
- Merge-result audit run against freshly fetched `origin/main` before
  handoff, the checked tip's SHA named in the handoff report (see
  Integration ordering and merge-result audit)
- `bash scripts/check.sh <component>` passes for every component in scope;
  for `scripts/` that includes its regression suites and the vocabulary
  check
- Decision note written or updated when the work hits a Decision notes
  trigger or the mandatory-note list; most work needs none

<!-- /agents-md:project:done-checks -->

<!-- /agents-md:managed:done -->

## Coordination

Coordination state lives in GitHub and git, never in status files. Issues
persist every work unit that outlives a direct, session-contained assignment;
this section holds the gates that govern finding, claiming, and finishing
one. Runtime AttentionItems (docs/plan.md §4) are a different system: this
section governs building Freeside, not running it.

Three rules govern parallel work:

1. A unit starts when it is authorized, nobody holds its claim, and every
   unit it lists as `starts-after` has merged.
2. A PR merges only after the required checks pass on its content combined
   with the tip it lands on.
3. A unit that must be written against another unit's result lists it as
   `starts-after`. Everything else, including a conflict in text, is
   resolved at landing, and after a rebase that conflicted, the author reads
   what the resolution changed.

Rule 2 proves what the required checks test and nothing else; Integration
Ordering and Merge-Result Audit holds the landing mechanics. Decision record:
`devlog/2026-10-26-1505-start-freely-validate-at-landing.md`.

The gates below bind every session and live here. The mechanics that
implement them (the work-unit issue shape, the claim-lease protocol, the
session-start queries, the migration protocol, reading a rebase, session end,
the escalation rules, and Freeside's additions to the tracker format) live in
[`docs/coordination.md`](docs/coordination.md). Read it before claiming a
unit, creating a work-unit issue, filing a deferral, starting an issue-backed
session, creating or updating a tracking issue, or starting any work that
carries dependencies or blockers.

### Work Units

Every work unit carries the contract defined under Default Agent Finish Line.
A direct, session-contained user assignment may keep it in the prompt and PR.
Scheduled work, backlog work, work that spans sessions, and work involving
more than one agent need a work-unit issue; when a direct task crosses one of
those boundaries mid-flight, promote it to an issue before continuing.
Scheduled self-selection (the scheduling door under Pickup in
docs/coordination.md) remains this project's explicit self-selection opt-in,
unchanged by the persistence rule.

One issue per issue-backed work unit, created from the work-unit template;
its Dependencies field takes only the typed relationships `starts-after`,
`merges-after`, and `stacked-on`, and an unknown or materially ambiguous
relationship is recorded as `starts-after` until the planner resolves it.
The template's fields, the labels and milestones, the §11 three-state wave
resolver (open issues carrying the `tracker` label and a milestone), and the
definition of **scheduled** (a milestone plus a listing on the current
tracking issue, set together by the planner) are in docs/coordination.md
§Work-Unit Issues. Every tracker's shape is in
[`docs/tracker-format.md`](docs/tracker-format.md), with Freeside's additions
under Tracking Issues in docs/coordination.md. Fiat (`Plan #N`, `Handle #N`)
is independent of wave state; the scheduling door exists only in active-wave
state.

The planner is the planning role: the owner, or a session the owner runs for
it. It assigns milestones and tracker listings, writes `starts-after` edges,
and runs the deferral sweep. It assesses no pairs and orders no integration;
the owner orders landing (Integration Ordering and Merge-Result Audit). A
`Plan #N` session is a planning session, not the planner: it makes only the
writes the Planning stage allows (Work-Unit Stages).

### Forge Edits

For routine issue, comment, plan, and tracker edits, accept a small risk of
recoverable concurrent-update errors. Ordinary care is sufficient:

- **Honor existing claims and reservations.** Coordinate with known
  overlapping writers, including writers to a shared tracker.
- **Read before editing and verify afterward.** Read the target and relevant
  baseline, preserve unrelated text, then verify the saved result. Read-back
  confirms the observed content; it cannot prove no concurrent edit was lost.
- **Recheck for a reason.** Refresh evidence when a change could affect the
  result. Don't repeatedly check unchanged state or restart planning for
  unrelated default-branch movement. This does not relax implementation's
  integration checks.
- **Repair from current records.** Reconcile observed conflicts and partial
  updates within authorized scope. Complete straightforward repairs; ask only
  when the resolution needs judgment. Report anything still incomplete, and
  never restore an old whole-body snapshot over someone else's edits.
- **Do not require unavailable forge guarantees.** Missing writer locks or
  atomic transactions do not block routine edits. No additional reservation
  scheme or permanent incomplete-state marker is required.
- **Keep consequential gates.** When an edit could start work or materially
  change authorization, apply the claim, relationship, and authorization
  checks below. Unverifiable required state and unresolved conflicts still
  block the dependent action. Never report an incomplete update as complete.

### Coordination Gates

These bind every session; docs/coordination.md holds the protocol that
implements and verifies each one. Each gate states what to check and for
which work. Keep that shape when editing, because a gate that names only a
condition is inert until something else tells you to go look.

- **Labels never authorize work.** An issue becomes agent-actionable through
  exactly two doors: scheduling (a planner sweep assigns its milestone and
  lists it on the current tracking issue, so this door is open only in
  active-wave state per the §11 resolver) or fiat (the human hands its number
  to a work-unit session, independent of wave state). Never select work by
  label or by browsing open issues.
- **`needs-human` is never agent-selected.** It stays unmilestoned and
  fiat-only, and returns to a session by fiat after the maintainer acts.
- **One claim per unit.** Before claiming, check the unit's own claims and
  planning reservation. After posting, repeat that query; among claims on the
  unit, the earliest forge-issued claim comment wins by `created_at`, then
  numeric comment ID. A losing claimant releases and stops. A claim or
  reservation on another unit never blocks yours. A bare cross-reference
  (`Refs #N`) is never a claim, and no new empty claim commits are created.
- **Claim state is verified, never assumed.** A comment or PR API read or
  write failure at any step fails closed: work does not begin, or continue
  past the failed step, while claim state cannot be verified.
- **Typed relationships govern start, landing order, and stacks.** Check
  every relationship before starting, whatever shape the work takes; a
  direct, session-contained assignment carries the same relationship contract
  as an issue-backed unit and reaches this gate without claiming anything.
  - `starts-after`: the prerequisite must be merged before the unit starts.
  - `merges-after`: never blocks start, for any unit. Name a prerequisite
    that has not merged in the handoff report; only the owner's approval
    order or `--order` to the landing batch enforces it (Integration Ordering
    and Merge-Result Audit).
  - `stacked-on`: names the intended base branch. Use that branch explicitly
    while the base PR is open and verify any existing child stays based
    there. After the base merges the relation is satisfied, but an existing
    child PR must be retargeted to the default branch before it can
    integrate.
  - Treat an unknown or materially ambiguous relationship as `starts-after`
    until the planner resolves it.
  - A leftover `exclusive-with` or contract-assessment line in an older
    issue is retired, not unknown: it gates nothing, and the planner deletes
    it when it next edits that field (`docs/coordination.md`, Relationship
    Types).
- **Start without comparing scope with open work.** Whatever shape the work
  takes, rule 1 is the whole start check: authorization, the unit's own
  claim, and its `starts-after` prerequisites. Don't compare your scope with
  other claims or open PRs, wait on another unit's reservation, or count
  open contract units. Overlapping edits and textual conflicts resolve at
  landing (rule 3).

### Contract Changes

Shared packages (domain types, migrations, the
StageDriver/ReviewSource/RunnerBackend interfaces, the API schema) change only
through `kind:contract` units, each in its own PR and merged before
dependents start: a unit written against a contract change lists it as
`starts-after` (rule 3). `kind:contract` is a classification that invokes
this contract review and verification; it implies no relationship to any
other unit and no limit on how many contract units are open. A contract PR
carries its required generated consumers and mechanical adapters (the
cross-component one-work-unit rule under Monorepo Scope Discipline). It may
also carry its first behavioral consumer, under the conditions in
`docs/coordination.md` (Unit Sizing); all other downstream feature work waits
for the merge. Other work never edits shared packages in passing: needing a
contract change means filing the contract issue and listing it as
`starts-after`.

Contract units need no coordination rule beyond rule 3, because of how a
contract change is made:

- **A breaking change fixes every user in the same PR.** Another open unit
  that still uses the old shape fails to compile or fails a test at landing.
- **A change of meaning is a rename in the same PR.** A field that keeps its
  shape but means something new gets a new name, so every open PR using the
  old name fails to compile at landing and its author has to look.
- **Stored data is the exception.** Rows and stored documents already exist
  under the old shape, so a change to them is added first and migrated.
- **Generated files meet at landing.** When another contract unit lands
  first, the second rebases and regenerates its consumers (Integration
  Ordering and Merge-Result Audit).
- **Migrations keep their numbering.** Take the next number; when another
  migration lands first, rename yours behind it at rebase; every migration
  after 0099 declares each name it removes with a `-- drop:` line, which
  `TestMigrationsDeclareDrops` enforces. Two migrations that rewrite the same
  rows, or rebuild one table, are written one against the other's result, so
  the planner orders them with `starts-after`. Detail: `docs/coordination.md`
  (Migrations).
