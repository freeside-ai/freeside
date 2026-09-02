# AGENTS.md

**Freeside** is an agent control plane: a local, durable workflow controller that grants agents the autonomy to turn work items into evidence-backed pull requests and interrupts a human only when judgment is required. The spec, architecture, and roadmap live in [`docs/plan.md`](docs/plan.md); read it first, and argue changes against it. This file holds the development conventions that apply to every session: decision notes, branch/PR/commit discipline, and the monorepo's scope rules.

Freeside is a monorepo. Each component directory (`daemon/`, `app/`, `api/`, `prompts/`, `policy/`, `images/`) stays empty until the phase that fills it, holding only a `README.md` stating its purpose until then; the per-component phase lives in that README and the roadmap (`docs/plan.md` §11). Do not scaffold a component ahead of its phase. "Empty" is not uniform: the API is provisional (plan §11 Wave 0; the decision record lives in docs/history/decisions.md), so drafting its skeleton in `api/` as a pre-1A design artifact is in scope, not a scope violation; `app/` starts with Phase 1A's minimal clients; the rest come in Phase 1A or later per their READMEs.

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

Routine implementation and coordination require no note. GitHub issues
and git remain the only sources of active work state; a note records
why, never status.

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

Freeside adopts planning and implementation as its work-unit stages. Review
convergence, the human merge gate, and post-merge cleanup remain phases of the
existing finish-line workflow, not additional stages. A stage adds no new
authorization door and weakens none of the claim, overlap, contract,
relationship, review, or merge gates elsewhere in this file.

### Planning

- **Activation:** Explicit owner fiat in the form `Plan #N`. A bare issue,
  label, claim, existing plan, or satisfied dependency does not activate the
  stage.
- **Allowed mutations:** After acquiring or recovering a reservation that is
  unexpired with enough remaining margin for the write and its post-write
  verification under `docs/coordination.md`, the stage may mutate the assigned
  issue's body as the authoritative work contract; its one versioned
  planning-reservation comment, replaced in place by the current
  implementation-plan comment or an explicit release marker; an edit or
  explicit non-current marker on a superseded plan comment; and trackers whose
  projections derive from an edited Dependencies field. Expiry ends
  planning-write authority; continuation requires a fresh reservation through
  that document's recovery procedure. A mutation whose verification finishes
  after expiry remains visible but unverified and never satisfies the planning
  finish line. The session may post only the recovery-only partial-state report
  that procedure requires; the report is not planning output or authority to
  continue planning. While active, the reservation blocks implementation of
  that issue and its direct `exclusive-with` partners; it is not a claim or an
  authorization door. No claim, branch, PR, code, or implementation change is
  allowed.
- **Required input:** The assigned issue and freshly resolved default-branch
  state; when changing Dependencies, the complete containing-tracker discovery
  and guarded projection-input set required by `docs/coordination.md`.
- **Durable output:** The completed issue-body contract and one current
  implementation-plan comment.
- **Finish line:** Both outputs are verified on the forge, with no claim or
  implementation started.
- **Transition:** Owner fiat hands the unit to implementation with `Handle #N,
  implementation plan in comments`; an already scheduled issue may instead
  enter implementation through the existing pickup door. Completing a plan
  alone never authorizes implementation.

### Implementation

- **Activation:** Explicit owner fiat in the form `Handle #N` for an
  issue-backed unit, a direct session-contained owner assignment with no
  issue, or pickup through the project's existing explicitly authorized
  scheduling door. When planning preceded it, the implementation input
  includes the plan in comments.
- **Allowed mutations:** The ordinary work-unit surface: its claim, isolated
  branch or worktree, declared paths, implementation and verification
  artifacts, review responses, and PR.
- **Required input:** The authoritative issue contract or, for a direct
  session-contained assignment, its prompt-backed work contract; when a
  planning stage ran, its single current implementation-plan comment. Execute
  that plan instead of replanning unless it conflicts with the work contract,
  this project's policy, dependencies, or current code reality; the
  authoritative source wins and the mismatch is surfaced.
- **Durable output:** An open, evidence-backed PR carrying the implementation
  and its verification record.
- **Finish line:** The Default agent finish line in this file: an open,
  review-ready PR with required checks green.
- **Transition:** A human decides whether to merge; after a verified merge,
  post-merge cleanup follows the record below.

## Merge Cleanup

### Post-merge obligations

- **Containing trackers:** For an issue-backed merged unit, identify every
  open tracker that lists its verified closing issue. Resolve the wave tracker
  per the §11 three-state resolver: in active-wave state it is the single open
  pinned title match and is a containing tracker when it lists the unit; in
  inter-wave state the only title match is the closed prior-wave tracker, which
  is never mutated, so no wave tracker is refreshed. Zero open containing
  trackers is a valid zero-work reconciliation result, not an
  incomplete-reconciliation error. A direct, session-contained unit has no
  containing tracker.
- **Refresh:** In each containing tracker, as one edit: tick the unit in the
  unit list, re-mark its diagram node with the merged double border when the
  tracker has a diagram, and recompute **Startable now** and **Mergeable
  next** as separate projections in the Implementation order.
- **Detailed mechanics:** `docs/coordination.md`.
- **Report:** Surface newly unblocked units without claiming or starting them,
  and identify integration evidence invalidated by the base advance.

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

## Build, test, run

The daemon (Wave 0 unit 1) and the API spec (Wave 0 unit 5) are initialized; the monorepo's other components are not. Per-component build, test, and run commands land in this table with each component's first PR (see `docs/plan.md` §11).

| Component     | Toolchain      | Commands                                      |
| ------------- | -------------- | --------------------------------------------- |
| `daemon/`     | Go             | `cd daemon`; `go build ./...`; `go test ./...`; `go vet ./...`; `golangci-lint run`; opt-in live suites are skipped by default and CI-blind (`FREESIDE_PUBLISH_LIVE_TEST`, `FREESIDE_WARD_LIVE_TEST`, `FREESIDE_CLAUDE_TOKEN_LIVE_TEST`, `FREESIDE_CODEX_ENROLLMENT_LIVE_TEST`, `FREESIDE_REAL_RUN_LIVE_TEST`; each test's skip message lists the rest of its environment) |
| `app/`        | Xcode / SPM    | `cd app`; `./scripts/generate-api-client.sh`; `swift test`; `xcrun swift-format lint --strict --recursive Sources Tests Apps Package.swift`; `xcodebuild -project Freeside.xcodeproj -scheme FreesideMac -destination 'platform=macOS' -skipPackagePluginValidation CODE_SIGNING_ALLOWED=NO build`; `xcodebuild -project Freeside.xcodeproj -scheme FreesideIOS -destination 'generic/platform=iOS Simulator' -skipPackagePluginValidation CODE_SIGNING_ALLOWED=NO build`; `bash scripts/run-convergence.sh` (repo root; §5.14 real-daemon convergence, builds the daemon harness); `./scripts/install-mac-app.sh --daemon-path </absolute/path/to/freesided> [--server-url <url>] [--launch]` installs or updates the operator's own signed FreesideMac and needs an `Apple Development` signing identity (see app/README.md); `./scripts/install-ios-app.sh --device <name-or-udid> [--server-url <url>] [--launch]` builds, signs, and installs FreesideIOS on a physical iPhone under free provisioning and needs Xcode plus a trusted device with Developer Mode (see app/README.md) |
| `api/`        | OpenAPI (spec) | `go run github.com/daveshanley/vacuum@v0.29.9 lint -r api/vacuum.ruleset.yaml --details --fail-severity warn api/openapi.yaml` (from repo root; see api/README.md) |
| `prompts/`    | prompt text    | not yet initialized; see docs/plan.md roadmap |
| `policy/`     | YAML (policy)  | not yet initialized; see docs/plan.md roadmap |
| `images/`     | OCI images     | `bash scripts/build-exporter-image.sh --local-registry-port 5000`; the agent bases are `bash scripts/build-agent-claude-image.sh --local-registry-port 5000` and `bash scripts/build-agent-codex-image.sh --local-registry-port 5000`, each followed by `bash scripts/check-agent-image.sh <ref>` on the registry-resolvable reference it prints (all need Apple `container`, and the builds need container egress; use `--registry HOST[/PATH]` for shared images); per-project images are runtime artifacts from the reusable builder (#334), manually proven before #237 and later invoked by `freesided onboard` (#238), not built from this directory |
| `scripts/`    | Bash / Go      | `bash -n scripts/*.sh app/scripts/*.sh`; `shellcheck scripts/*.sh app/scripts/*.sh`; `bash scripts/test-build-image-references.sh`; `bash scripts/test-check-commit-messages.sh`; `bash scripts/test-merge-result-audit.sh`; `bash scripts/test-check-agent-image.sh`; `bash scripts/test-install-mac-app.sh`; `bash scripts/test-install-ios-app.sh` (CI pins shellcheck in `.github/workflows/scripts-ci.yml`); `go -C scripts/trackercollect build ./...`; `go -C scripts/trackercollect test ./...`; `go -C scripts/trackercollect vet ./...`; `bash scripts/link-plan-section-refs.sh --check` verifies that every "Section N" citation in `docs/plan.md` links to its heading anchor and that no citation or existing link is broken (drop `--check` to rewrite the links after a plan edit), with `bash scripts/test-link-plan-section-refs.sh` as its regression suite; `bash scripts/check-vocabulary.sh` (repo root) refuses the pre-rename stage vocabulary in daemon non-test Go code, migrations after 0064, `api/`, `app/`, `prompts/`, and `scripts/` (one `legacy_vocabulary.go` per package is the only place the old literals may live; CI runs it), with `bash scripts/test-check-vocabulary.sh` as its regression suite; `bash scripts/run-real-work.sh <spec> <policy> <publication>` is the §11 1A.2 real unattended run and needs Apple `container` plus the operator preconditions its header lists |

Lint/format and CI are established with the first component that carries code: the daemon does so here via `daemon/.golangci.yml` and `.github/workflows/daemon-ci.yml` (Linux runs build/test/vet/lint, macOS runs build/test). Later components add their own on the same pattern.

## Daemon coding conventions

Binding for new and changed `daemon/` Go code, promoted at Wave 0 exit
(#27) from the domain package's point-of-use conventions; the detail
lives at point-of-use, not here. The promotion is a ratchet, not a
retroactive claim: a pre-promotion deviation gets a tracker issue and
drains as its own unit, never a fix in passing (Monorepo scope
discipline).

- **Enums**: a named string type with a `valid()` predicate and an
  `AllX` slice as the single registration point; the zero value `""` is
  invalid by design. (Detail: `daemon/internal/domain/doc.go`.)
- **Switches over enums**: a validity `valid()` switch uses `default`
  (it is a predicate); a switch that dispatches behaviour omits
  `default` so the `exhaustive` linter (`default-signifies-exhaustive:
  true` in `daemon/.golangci.yml`) forces a new member to be handled,
  with a trailing fallback return for the invalid zero value.
- **Golden tests**: `json.MarshalIndent` of a fixed, valid fixture
  (UTC-fixed times, pointer-for-optional rendering explicit null, no
  map fields in the contract shapes goldens pin; a package-private
  persistence format is not one); fixtures double as
  validation-positive cases. (Worked example: `daemon/README.md`;
  shared helper: `daemon/internal/golden`.)
- **Trust boundaries at reconstruction/persistence**: a boundary that
  decodes a row or accepts an exported struct re-runs the trusted
  policy gate against current state (e.g. the approved-recipe set); a
  decoded or caller-supplied trust bit (`publish_eligible`, recipe
  approval, a provenance head) is never trusted, and the re-gate fails
  closed. Promoted per #52 when the invariant recurred beyond the
  store. (Detail: `daemon/internal/store/entities.go`,
  `daemon/internal/domain/artifact.go`.)
- **Timer-dependent tests**: a test whose behavior depends on real
  stdlib time in the code under test (`time.Timer`/`time.Ticker`/
  `time.After`/`context` deadline) runs inside a `testing/synctest`
  bubble, not a real-clock sleep or poll. A ratchet on new or
  substantially revised tests, not a retrofit sweep, and only where the
  code uses the real `time` package; injected-clock behavior (the
  scheduler's occurrence-due logic, the janitor and engine `now`) is
  already deterministic. (Detail: `daemon/README.md`; worked example:
  `daemon/internal/scheduler/run_synctest_test.go`.)

## Monorepo scope discipline

A work unit declares which component directories it touches, in the branch-name context and the PR body, and does not modify directories outside that declared scope. This is the manual precursor of Freeside's control-plane path restrictions (`docs/plan.md` §5.6, §5.8) and will later be enforced mechanically by the importer.

- Name the touched components in the PR body (a one-line "Scope:" is enough).
- Cross-component changes (typically `api/` plus both of its consumers, `daemon/` and `app/`) are **one work unit** and must say so; a spec change and its generated-code consumers move together, never in silently coupled separate PRs.
- Do not edit a component outside the current unit's declared scope to "fix while you're here"; file it instead.

## Document gating

Changes to `docs/plan.md`, ADRs (`docs/decisions/`), and (later) the control-plane directories (`policy/`, `prompts/`) are reviewed like code, gated by **materiality** (`docs/plan.md` §9). Material changes — scope, acceptance criteria, milestones, sequencing affecting active work, architecture, risk posture, commitments — are **never batched silently into a feature PR**; wording and clarification changes are recorded in the PR that carries them, not separately gated.

- A material plan change is its own PR, unless the plan change *is* the direct subject of the feature PR (then it is called out explicitly in the PR body).
- ADRs are promoted from decision notes (`docs/decisions/README.md`); the promotion is its own reviewed change.
- The materiality rules themselves are control-plane policy (plan §9); changing them is a material change.

## Markdown conventions

Markdown headings use **title case** at every level (`docs/intro.md` is the reference example). Convergence is a **ratchet, not a retroactive sweep**: new docs and any heading a substantial revision already touches adopt title case; existing sentence-case docs (`docs/plan.md`, this file, the component READMEs) stay as they are until a substantive revision brings them along, and a heading-only retitle of an otherwise-unchanged doc is not a work unit. The point is that no doc drifts *away* from title case, not that every doc is converted at once.

A heading whose exact spelling is a machine-read record identifier preserves
that spelling; for example, `### Post-merge obligations` is an interface token,
not ordinary prose to retitle.

## Automated reviewer

**Codex** reviews pull requests automatically. Respond to its findings per **Responding to automated review** under Pull requests, and filter later review activity by its login.

- **Login/account:** `chatgpt-codex-connector` (the `chatgpt-codex-connector[bot]` form appears on inline review comments and in the pulls review-comments API).
- **Triggered:** automatically on PR open-for-review, mark-ready, and each push (it re-reviewed after every push this session); also on demand via an `@codex review` comment.
- **Status signals:** Codex maintains one `codex-pull-request-review-summary` comment: it posts the comment on the first trigger and updates it in place on later triggers, with the reviewed commit, trigger, and completion time. On a **clean pass** (no findings) it posts no review and reacts 👍 (`+1`, i.e. `THUMBS_UP`) on the PR description a few minutes after the triggering event; that reaction, dated after the trigger, is the completion signal a review-watch keys off. On a **findings pass** it posts a `COMMENTED` review whose inline comments are each tagged by priority badge (P1/P2/P3) and invite a 👍/👎 reaction.

## Freeside Review-Loop Bound

The managed review-convergence guidance runs under Freeside's resolved policy:
round counts are emergency brakes, not the normal stopping rule, but policy
exhaustion remains a mandatory stop. When the bound is exhausted or the result
is ambiguous, stop and create a durable AttentionItem; thrash is an additional
stop condition, not a replacement for policy exhaustion or escalation.

## Integration ordering and merge-result audit

Freeside's mechanical defense for the integration-evidence invariant
(**Integration evidence belongs to one base commit**, under Pull
requests): a branch carrying stale or inverse content can silently
revert already-merged sibling work through a clean 3-way merge (#47
reverting #48; recovered in #49). The audit constructs the prospective
merge result against the current base tip without mutating the
checkout and enforces the unit's declared path scope on it.

- The spine role owns final integration ordering when multiple PRs are
  ready; a work unit's Dependencies field records typed `merges-after`
  integration constraints and `stacked-on` intentional bases (see Stacked
  PRs).
- After any merge to `main`, every remaining open PR's integration
  evidence is stale until revalidated against the new tip.
- Before final handoff, and again after any base advance: fetch the
  default branch, run
  `scripts/merge-result-audit.sh origin/main <head-branch> <allowed-path>...`
  against that exact tip, review the complete prospective change set it
  prints, and record the resolved base SHA plus the audit command and
  verdict in the PR's Verification section.
- Allowed paths are the unit's declared scope, passed explicitly; the
  audit never parses PR prose. Its guarantees are conflict detection,
  exact-base binding, complete prospective-diff visibility, and
  path-boundary enforcement; it does not infer semantic intent, so an
  in-scope reversion still needs a reviewer's eyes on the printed
  change set.

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

Pull-request CI runs
`bash scripts/check-commit-messages.sh <base-ref> <head-ref>` (locally, usually
`bash scripts/check-commit-messages.sh origin/main HEAD`). It resolves the
merge base and checks every non-merge commit in `merge-base..head`; merge
commits and mainline commits brought in by a base-freshness merge are exempt.
The check reports every offending commit and rule in one run.

Every checked commit must satisfy all of these mechanical rules:

- A subject and a body are required, with line 2 blank between them. The
  body must contain at least one non-blank line after that separator.
- The subject is at most 72 characters, does not end in a period, and does
  not begin with a lowercase ASCII letter. Acronym-, identifier-, and
  digit-led subjects remain valid.
- Case-insensitive Conventional Commit prefixes are forbidden for `build`,
  `chore`, `ci`, `docs`, `feat`, `fix`, `perf`, `refactor`, `revert`, `style`,
  and `test`, including scoped and breaking forms such as `feat(api):` and
  `refactor!:`.
- Case-insensitive `fixup!` and `squash!` prefixes and standalone `WIP`
  markers are forbidden.
- Case-insensitive review-cleanup prefixes are forbidden: `Address review`,
  `Address PR review`, `Address pull request review`, `Apply review feedback`
  (with optional `PR` or `pull request` before `review`), `PR feedback`, and
  `Pull request feedback`. Fold that work into its owning commit instead.
- Body lines are at most 72 characters. A line with no whitespace is exempt
  so an unbreakable URL, object ID, or ref can remain intact.

<!-- agents-md:managed:done -->

## Definition of Done for an Increment

An increment is done only when it's running and exercised by the end of the
work session. "Code complete" or passing tests alone isn't enough.

Before calling the work done, confirm that the build succeeds, tests pass,
and lint and formatting are clean.

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
- Merge-result audit run against freshly fetched `origin/main` before
  handoff, base SHA and verdict recorded in PR Verification (see
  Integration ordering and merge-result audit); when `scripts/` is in
  scope, `bash scripts/test-build-image-references.sh`,
  `bash scripts/test-check-commit-messages.sh`,
  `bash scripts/test-merge-result-audit.sh`,
  `bash scripts/test-check-agent-image.sh`,
  `bash scripts/test-install-mac-app.sh`,
  `bash scripts/test-install-ios-app.sh`, and
  `bash scripts/test-check-vocabulary.sh` also pass, and
  `bash scripts/check-vocabulary.sh` is clean
- Decision note written or updated when the work hits a Decision notes
  trigger or the mandatory-note list; most work needs none

<!-- /agents-md:project:done-checks -->

<!-- /agents-md:managed:done -->

## Coordination

Coordination state lives in GitHub and git, never in status files. Issues
persist every work unit that outlives a direct, session-contained
assignment; this section holds the gates that govern finding, claiming, and
finishing one. Runtime AttentionItems (docs/plan.md §4) are a different
system; this section governs building Freeside, not running it.

The gates below bind every session and live here. The mechanics that
implement them (the lane glossary, the claim-lease protocol, the
session-start queries, session end, the escalation routing rules, and the
tracking-issue format) live in
[`docs/coordination.md`](docs/coordination.md); read it before claiming a
unit, filing a deferral, starting an issue-backed session, creating or
updating a tracking issue, or starting any work that carries dependencies
or blockers.

### Work Units

Every work unit carries the lightweight work contract the finish line
defines (objective, testable acceptance criteria, scope, dependencies and
blockers, explicit non-goals); this section governs where that contract
persists. A direct, session-contained user assignment may carry the
contract in the prompt and PR together. Scheduled work,
backlog work, work that spans sessions, and work involving more than one
agent require a work-unit issue; when a direct task crosses one of those
boundaries mid-flight, promote it to an issue before continuing.
Scheduled self-selection (the scheduling door under Pickup in
docs/coordination.md) remains this project's explicit self-selection
opt-in, unchanged by the persistence rule.

One issue per issue-backed work unit, created from the work-unit
template: Source devlog entry (optional; cite the originating decision
note's filename only when the issue genuinely originated in one),
Objective, Non-goals (`none` allowed), Affected
interfaces/contracts (the interface surfaces the unit touches, not the
whole work contract; the issue as a whole is the contract), Acceptance
(the fixture/test list is the spec), Scope / declared paths, Dependencies
(`starts-after`, `merges-after`, `stacked-on`, and `exclusive-with`, not
untyped issue refs). Unknown or materially ambiguous relationships are
recorded as `starts-after` until the spine resolves them.
Labels: `lane:*` for ownership area, `kind:*` for type. Milestones carry the
phase (1A, 1B). Each wave has a pinned tracking issue listing its units; the
spine role maintains it. Any issue that tracks other issues (a wave tracker
or an ad hoc tracker over a set of units) records their implementation
order per the tracking-issue format in docs/coordination.md.

Wave state resolves per the §11 three-state resolver over every pinned issue
whose title matches the canonical wave-tracker pattern: exactly one open match
is active-wave state, exactly one closed match is inter-wave state, and zero or
multiple matches are an invalid authority state for spine repair. The
scheduling door exists only in active-wave state, because it needs an open
current tracker to list the unit; fiat (`Plan #N`, `Handle #N`) is independent
of wave state and may proceed in either active-wave or inter-wave state after
all ordinary gates pass.

Here and below, **scheduled** means both a milestone and a listing on the
current tracking issue. The spine changes those fields as one planning
operation; either field alone is a spine-repair error and does not open the
scheduling door (fiat remains independent).

### Lane names

Lane names are search keys and territory labels. They never appear in code
identifiers, package names, or API vocabulary, which stay functional (the
attention type is AttentionItem, not SignetItem). The canonical lane table,
with each lane's owned paths, is in docs/coordination.md.

### Coordination Gates

These bind every session. The protocol that implements them, including how
to verify each one, is in docs/coordination.md. Each gate states what to
check and for which work; keep that shape when editing, because a gate that
names only a condition is inert until something else tells you to go look.

- **Labels never authorize work.** An issue becomes agent-actionable through
  exactly two doors: scheduling (a spine sweep assigns its milestone and
  lists it on the current tracking issue, so this door is open only in
  active-wave state per the §11 resolver) or fiat (the human hands its number
  to a work-unit session, independent of wave state). A session must never
  select work directly by label or by browsing open issues.
- **`needs-human` is never agent-selected.** It stays unmilestoned and
  fiat-only, and returns to a session by fiat after the maintainer acts.
- **One claim per unit, with exclusivity arbitration.** Check the current unit
  and every forward or reverse `exclusive-with` partner before claiming. After
  posting, repeat the complete query; among conflicting claims, the earliest
  forge-issued claim comment wins by `created_at`, then numeric comment ID. A
  losing claimant releases and stops. A bare cross-reference (`Refs #N`) is
  never a claim, and no new empty claim commits are created.
- **Claim state is verified, never assumed.** A comment or PR API read or
  write failure at any step fails closed: work does not begin, or continue
  past the failed step, while claim state cannot be verified.
- **Typed relationships govern start, integration, stacks, and exclusivity.**
  Check every relationship before starting, whatever shape the work takes: a
  direct, session-contained assignment carries the same relationship contract
  as an issue-backed unit, and reaches this gate without claiming anything.
  A `starts-after` prerequisite must be merged before the unit starts. A
  `merges-after` prerequisite never blocks start, but must be rechecked at
  handoff and before integration. A `stacked-on` relation names its intended
  base branch: use that branch explicitly while the base PR is open and verify
  any existing child remains based there; after the base merges, the relation
  is satisfied, but an existing child PR must be retargeted to the default
  branch before it can integrate. An `exclusive-with` declaration on either
  unit forbids both units from being active concurrently. Before starting,
  check both the current unit's declarations and reverse `exclusive-with`
  declarations in every open work-unit issue, then run the cross-unit claim
  arbitration. Before adding an `exclusive-with` declaration, the editor
  checks both endpoints and must not make the edit while any claim or foreign
  planning reservation is active; a planning transaction may retain only its
  own unexpired reservation with sufficient write-and-verification margin on
  its assigned endpoint. The editor waits for the blocking record to release
  before the relationship changes. A declaration appearing during claim
  arbitration makes that claimant stop until the edit and a fresh relationship,
  claim, and reservation read complete. Treat an unknown or materially
  ambiguous relationship as `starts-after` until the spine resolves it.
- **Check open PRs for declared-path overlap before you start.** Compare
  every open PR's declared paths against yours, whatever shape the work
  takes; an overlap means stop and coordinate via issue comment before going
  further.
- **Contract work serializes.** Check open `kind:contract` issues before you
  start, whatever shape the work takes: one touching the shared-package
  surfaces your work will change blocks you. An issue-backed unit names
  those in its Affected interfaces/contracts field; a direct assignment
  derives them from its declared scope. Claiming a `kind:contract` unit
  additionally blocks on every other open contract unit, excluding the one
  you are claiming and any whose `starts-after` chain includes it, so a
  `starts-after` contract chain keeps its head claimable. A `deferral`-labelled
  contract unit counts only once it is scheduled or actively claimed.

### Contract changes

Shared packages (domain types, migrations, StageDriver/ReviewSource/
RunnerBackend interfaces, the API schema) change only through
`kind:contract` units: spine-owned, their own PR, under a standing
`exclusive-with` regime against every other contract unit, and merged before
dependents start. A contract PR carries its required
generated consumers and mechanical adapters (the cross-component
one-work-unit rule under Monorepo scope discipline); only downstream
feature work waits for the merge. Lane work never edits shared packages
in passing; needing a contract change means filing the contract issue,
linking it as a dependency, and blocking or switching units.

Before a `kind:contract` deferral is scheduled or assigned by fiat, the spine
inserts it into the serialized contract `starts-after` chain; if it has no
valid position, it stays dormant. Fiat never bypasses contract ordering.
