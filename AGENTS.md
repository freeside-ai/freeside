# AGENTS.md

**Freeside** is an agent control plane: a local, durable workflow controller that grants agents the autonomy to turn work items into evidence-backed pull requests and interrupts a human only when judgment is required. The spec, architecture, and roadmap live in [`docs/plan.md`](docs/plan.md); read it first and argue changes against it. This file holds the conventions that apply to every session: decision notes, branch, PR, and commit discipline, and the monorepo's scope rules.

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
  dependency updates, mechanical syncs, and simple fixes need no note unless
  they reveal a lasting decision or discovery.
- **Record the final reasoning.** Include rejected options, changed
  assumptions, important verification findings, and a "Revisit when ..."
  condition where one is useful. Do not include diffs, test logs, chronology,
  or PR status.
- **Let an active note evolve.** Update it while its work unit or PR is open.
  Freeze it when the PR merges.
- **Find notes from the work first.** Read notes linked from the current issue
  or PR. Otherwise, search by path, topic, contract, or decision name. Read
  the latest note only when resuming the work unit it describes.
- **Treat old notes as evidence, not rules.** Do not silently overturn an
  explicit owner decision. When new evidence conflicts with one, name the old
  decision, explain which assumption or condition changed, and propose the
  revision.
- **Track deferred work in issues.** Link the note from an issue that starts
  there. The note may keep a historical `Follow-up: #N` link, but never a
  second status record. Put non-actionable observations in "Revisit when ...".

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

- **Current shape:** Shapes 2–5 combined: path-and-dependency units, typed
  relations, stable named work streams, and an integration
  spine/shared-contract domain. Current demonstrated width is four fronts,
  further bounded by each wave tracker, live claims, path overlap, review
  bandwidth, and spine integration capacity.
- **Evidence basis:** Waves 3–6 and issue/PR history show recurring
  lane-scoped fronts; PR #801 established the typed relations; Wave 6 tracker
  #835 runs a repo-wide-exclusive contract chain alongside independent
  fronts and identifies review bandwidth as the binding constraint.
- **Detailed mechanics:** [`docs/coordination.md`](docs/coordination.md).
- **Reassess when:** A planned wave needs more than four fronts;
  review or integration queueing persists at four or fewer; work repeatedly
  crosses lane boundaries; or shared-contract serialization or the spine role
  materially changes.

## Work-Unit Stages

Freeside's work-unit stages are planning and implementation. Review
convergence, the human merge gate, and post-merge cleanup stay phases of the
finish-line workflow, not stages. A stage adds no authorization door and
weakens none of the claim, overlap, contract, relationship, review, or merge
gates in this file.

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
  implementation of the issue and its direct `exclusive-with` partners; it is
  not a claim or an authorization door. No claim, branch, PR, code, or
  implementation change is allowed.
- **Required input:** The assigned issue and freshly resolved default-branch
  state; when changing Dependencies, the complete containing-tracker discovery
  and guarded projection-input set that `docs/coordination.md` requires.
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
  tracker that lists its verified closing issue. Resolve the wave tracker with
  the §11 three-state resolver: in active-wave state it is the single open
  pinned title match, and it is a containing tracker when it lists the unit;
  in inter-wave state the only title match is the closed prior-wave tracker,
  which is never mutated, so no wave tracker is refreshed. Zero open
  containing trackers is a valid zero-work result, not an
  incomplete-reconciliation error. A direct,
  session-contained unit has no containing tracker.
- **Refresh:** In each containing tracker, as one edit: tick the unit in the
  unit list, re-mark its diagram node with the merged double border when the
  tracker has a diagram, and recompute **Startable now** and **Mergeable
  next** as separate projections in the Implementation order.
- **Detailed mechanics:** `docs/coordination.md`.
- **Report:** Name newly unblocked units without claiming or starting them,
  and name the integration evidence the base advance invalidated.

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
3. Make the scoped change. Include the docs, tests, and assets needed to keep
   it complete. Add a decision note only when its triggers apply.
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

`bash scripts/check.sh <component> [step...]` runs a component's standard
checks from the repo root, and CI runs those steps through the same script,
so its step table is the authoritative check list; the one exception is
daemon lint, which CI runs through golangci-lint-action at the same pinned
version. `bash scripts/check.sh --list` prints the components (`daemon`,
`app`, `api`, `scripts`, `convergence`, `docs`) and their steps; the script
header documents the tool overrides and the daemon's opt-in live suites. Operator
tools that are not checks live with their component: installers in
app/README.md, image builds in images/README.md, the real unattended run in
the `scripts/run-real-work.sh` header, and the API linter pin in
api/README.md. `prompts/` and `policy/` are not yet initialized. Lint,
format, and CI follow the daemon's pattern, `daemon/.golangci.yml` and
`.github/workflows/daemon-ci.yml` (Linux runs build, test, vet, and lint;
macOS runs build and test); each new component adds its own on that pattern
in its first PR and registers its steps in `scripts/check.sh`.

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

Headings use **title case** at every level (`docs/intro.md` is the reference
example). Convergence is a ratchet, not a retroactive sweep: new docs and any
heading a substantial revision already touches adopt title case; existing
sentence-case docs (`docs/plan.md` and the component READMEs) keep theirs
until a substantive revision brings them along, and a heading-only retitle of
an otherwise unchanged doc is not a work unit. No doc drifts away from title
case; not every doc converts at once.

A heading whose exact spelling is a machine-read record identifier keeps that
spelling; `### Post-merge obligations` and `### Coordination model` are
interface tokens, not prose to retitle.

## Automated Reviewer

**Codex** reviews pull requests automatically. Handle its findings per the
review bullets under Pull Requests (judge on merits, reply after the pushed
fix, fix the whole class), and filter later review activity by its login.

- **Login/account:** `chatgpt-codex-connector`; the
  `chatgpt-codex-connector[bot]` form appears on inline review comments and in
  the pulls review-comments API.
- **Triggered:** on PR open-for-review, mark-ready, and each push; on demand
  via an `@codex review` comment.
- **Status signals:** Codex keeps one `codex-pull-request-review-summary`
  comment, posted on the first trigger and updated in place afterwards with
  the reviewed commit, trigger, and completion time. On a **clean pass** (no
  findings) it posts no review and reacts 👍 (`+1`, `THUMBS_UP`) on the PR
  description a few minutes after the trigger; that reaction, dated after the
  trigger, is the completion signal a review watch keys off. On a **findings
  pass** it posts a `COMMENTED` review whose inline comments each carry a
  priority badge (P1/P2/P3) and invite a 👍/👎 reaction.

## Freeside Review-Loop Bound

The managed review-convergence guidance runs under Freeside's resolved policy:
round counts are emergency brakes, not the normal stopping rule, but policy
exhaustion is still a mandatory stop. When the bound is exhausted or the
result is ambiguous, stop and create a durable AttentionItem. Thrash is an
additional stop condition, not a replacement for policy exhaustion or
escalation.

## Integration Ordering and Merge-Result Audit

Integration evidence belongs to one base commit (the "Repeat integration
checks when the base moves" rule under Pull Requests). The merge-result audit
is its mechanical defense; the script header carries the incident it answers
and its guarantees.

- The spine role owns final integration ordering when several PRs are ready.
  A unit's Dependencies field records typed `merges-after` integration
  constraints and `stacked-on` intentional bases (see Stacked PRs).
- After any merge to `main`, every remaining open PR's integration evidence is
  stale until revalidated against the new tip.
- Before final handoff, and again after any base advance: fetch the default
  branch and run
  `scripts/merge-result-audit.sh origin/main <head-branch> <allowed-path>...`
  against that exact tip, with the unit's declared scope as the allowed
  paths. Read the whole change set it prints: the audit cannot see intent,
  so an in-scope reversion still passes. Record the resolved base SHA, the
  audit command, and the verdict in the PR's Verification section.

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
- **Use the PR template for the body.** Include Why, What, Screenshots for UI
  changes, optional Review Notes, and Verification. Key the commit map by
  subject, not SHA. Start verification bullets with `Passed:`, `Checked:`,
  `Attempted:`, or `Not run:`. Before writing or updating the body, read
  `docs/agent-workflow.md` §pr-body. For a UI change, meet its Screenshots
  requirements.
- **Self-review the full diff in the PR files view.** Look for stray changes,
  debug code, scope creep, and accidental files. This catches accidental
  changes; it doesn't check whether the solution is correct.
- **Repeat integration checks when the base moves.** CI, final diff review,
  and readiness count only for the base commit you checked. Repeat all three
  if the base changes.
- **Use fresh eyes for substantive review.** Reviewing your own work in the
  same conversation shares the author's blind spots. A review in a fresh
  conversation is more independent. A bot from another provider or a human is
  stronger. Rely on a bot or human before handoff. For non-trivial work, or
  without a bot reviewer, read `docs/agent-workflow.md` §pre-push-review before
  pushing.
- **Record an automated reviewer you observe.** If the project has no record
  for that reviewer or signal, read `docs/agent-workflow.md` §reviewer-record
  and update the project record before handoff.
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
  update What, the subject-based commit map, and Verification. Mark commits
  that resolve review findings. Keep each finding's outcome in its inline
  reply, not a permanent feedback section.
- **Keep the intended repository rules.** Use merge commits only, disable
  squash and rebase merges, use title-only merge messages, and auto-delete
  merged branches. Do not re-enable a disabled method. Enforce these rules
  manually where repository settings don't.

### Handing Off the PR

A PR is ready to hand off when it's open, green, self-reviewed, has no
unhandled threads, and has no outstanding review activity. After opening the
PR, read `docs/agent-workflow.md` §handing-off and follow its sequence:

1. Start the review watch from the PR open or push event. Only reviewer
   activity after that event counts as new. After another push, start counting
   from that push.
2. Refresh from the current base and record the base commit.
3. Wait for required checks. Never hand off known-red work.
4. Self-review the final diff.
5. Close the watch by handling findings or recording its bounded timeout.
6. Stop and summarize for the human reviewer.

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

Before your first commit in a clone, run `git config core.hooksPath
.githooks`: the commit hook gives a best-effort early check on the normal
editor/strip-cleanup path. Git does not give that hook its later cleanup mode
or the effective `core.commentChar=auto` character, so pull-request CI is the
exact authority. The rules (subject and body shape, 72-character limits, and
the forbidden Conventional Commit, autosquash, WIP, and review-cleanup
prefixes) are in the header of `scripts/check-commit-messages.sh`. CI runs the
script over every non-merge commit in `merge-base..head`, and
`bash scripts/check-commit-messages.sh origin/main HEAD` checks a branch
locally. The same `core.hooksPath` opt-in also enables the `pre-commit` hook
that regenerates the tracked API client when a commit stages one of its
inputs; CI's `generate` job stays the backstop.

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
  handoff, base SHA and verdict recorded in PR Verification (see
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

The gates below bind every session and live here. The mechanics that
implement them (the lane glossary, the work-unit issue shape, the claim-lease
protocol, the session-start queries, session end, the escalation routing
rules, and the tracking-issue format) live in
[`docs/coordination.md`](docs/coordination.md). Read it before claiming a
unit, creating a work-unit issue, filing a deferral, starting an issue-backed
session, creating or updating a tracking issue, or starting any work that
carries dependencies or blockers.

### Work Units

Every work unit carries the work contract the finish line defines (objective,
testable acceptance criteria, scope, dependencies and blockers, explicit
non-goals); this section governs where that contract persists. A direct,
session-contained user assignment may carry it in the prompt and PR together.
Scheduled work, backlog work, work that spans sessions, and work involving
more than one agent need a work-unit issue; when a direct task crosses one of
those boundaries mid-flight, promote it to an issue before continuing.
Scheduled self-selection (the scheduling door under Pickup in
docs/coordination.md) remains this project's explicit self-selection opt-in,
unchanged by the persistence rule.

One issue per issue-backed work unit, created from the work-unit template;
its Dependencies field takes only the typed relationships `starts-after`,
`merges-after`, `stacked-on`, and `exclusive-with`, and an unknown or
materially ambiguous relationship is recorded as `starts-after` until the
spine resolves it. The template's fields, the labels and milestones, the §11
three-state wave resolver, and the definition of **scheduled** (a milestone
plus a listing on the current tracking issue, set together by the spine) are
in docs/coordination.md §Work-Unit Issues. Fiat (`Plan #N`, `Handle #N`) is
independent of wave state; the scheduling door exists only in active-wave
state.

### Lane Names

Lane names are search keys and territory labels. They never appear in code
identifiers, package names, or API vocabulary, which stay functional (the
attention type is AttentionItem, not SignetItem). The canonical lane table,
with each lane's owned paths, is in docs/coordination.md.

### Coordination Gates

These bind every session; docs/coordination.md holds the protocol that
implements and verifies each one. Each gate states what to check and for
which work. Keep that shape when editing, because a gate that names only a
condition is inert until something else tells you to go look.

- **Labels never authorize work.** An issue becomes agent-actionable through
  exactly two doors: scheduling (a spine sweep assigns its milestone and lists
  it on the current tracking issue, so this door is open only in active-wave
  state per the §11 resolver) or fiat (the human hands its number to a
  work-unit session, independent of wave state). Never select work by label
  or by browsing open issues.
- **`needs-human` is never agent-selected.** It stays unmilestoned and
  fiat-only, and returns to a session by fiat after the maintainer acts.
- **One claim per unit, with exclusivity arbitration.** Before claiming, check
  the current unit and every forward or reverse `exclusive-with` partner.
  After posting, repeat the complete query; among conflicting claims, the
  earliest forge-issued claim comment wins by `created_at`, then numeric
  comment ID. A losing claimant releases and stops. A bare cross-reference
  (`Refs #N`) is never a claim, and no new empty claim commits are created.
- **Claim state is verified, never assumed.** A comment or PR API read or
  write failure at any step fails closed: work does not begin, or continue
  past the failed step, while claim state cannot be verified.
- **Typed relationships govern start, integration, stacks, and exclusivity.**
  Check every relationship before starting, whatever shape the work takes; a
  direct, session-contained assignment carries the same relationship contract
  as an issue-backed unit and reaches this gate without claiming anything.
  - `starts-after`: the prerequisite must be merged before the unit starts.
  - `merges-after`: never blocks start, but recheck it at handoff and before
    integration.
  - `stacked-on`: names the intended base branch. Use that branch explicitly
    while the base PR is open and verify any existing child stays based
    there. After the base merges the relation is satisfied, but an existing
    child PR must be retargeted to the default branch before it can
    integrate.
  - `exclusive-with`: a declaration on either unit forbids both from being
    active concurrently. Before starting, check the current unit's
    declarations and reverse declarations in every open work-unit issue, then
    run the cross-unit claim arbitration.
  - Adding an `exclusive-with` declaration: the editor checks both endpoints
    and must not edit while any claim or foreign planning reservation is
    active; a planning transaction may retain only its own unexpired
    reservation, with sufficient write-and-verification margin, on its
    assigned endpoint. The editor waits for the blocking record to release
    before changing the relationship. A declaration that appears during claim
    arbitration stops that claimant until the edit completes and a fresh
    relationship, claim, and reservation read is done.
  - Treat an unknown or materially ambiguous relationship as `starts-after`
    until the spine resolves it.
- **Check open PRs for declared-path overlap before you start.** Compare
  every open PR's declared paths against yours, whatever shape the work takes.
  An overlap means stop and coordinate via issue comment before going further.
- **Contract work serializes.** Before you start, whatever shape the work
  takes, check open `kind:contract` issues: one touching the shared-package
  surfaces your work will change blocks you. An issue-backed unit names those
  surfaces in its Affected interfaces/contracts field; a direct assignment
  derives them from its declared scope. Claiming a `kind:contract` unit
  additionally blocks on every other open contract unit, excluding the one you
  are claiming and any whose `starts-after` chain includes it, so a
  `starts-after` contract chain keeps its head claimable. A
  `deferral`-labelled contract unit counts only once it is scheduled or
  actively claimed.

### Contract Changes

Shared packages (domain types, migrations, the
StageDriver/ReviewSource/RunnerBackend interfaces, the API schema) change only
through `kind:contract` units: spine-owned, in their own PR, under a standing
`exclusive-with` regime against every other contract unit, and merged before
dependents start. A contract PR carries its required generated consumers and
mechanical adapters (the cross-component one-work-unit rule under Monorepo
Scope Discipline); only downstream feature work waits for the merge. Lane work
never edits shared packages in passing: needing a contract change means filing
the contract issue, linking it as a dependency, and blocking or switching
units.

Before a `kind:contract` deferral is scheduled or assigned by fiat, the spine
inserts it into the serialized contract `starts-after` chain; with no valid
position it stays dormant. Fiat never bypasses contract ordering.
