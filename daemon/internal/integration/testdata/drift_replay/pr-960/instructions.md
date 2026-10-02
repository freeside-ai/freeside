# AGENTS.md

**Freeside** is an agent control plane: a local, durable workflow controller that grants agents the autonomy to turn work items into evidence-backed pull requests and interrupts a human only when judgment is required. The spec, architecture, and roadmap live in [`docs/plan.md`](docs/plan.md); read it first, and argue changes against it. This file holds the development conventions that apply to every session: decision notes, branch/PR/commit discipline, and the monorepo's scope rules.

Freeside is a monorepo. Each component directory (`daemon/`, `app/`, `api/`, `prompts/`, `policy/`, `images/`) stays empty until the phase that fills it, holding only a `README.md` stating its purpose until then; the per-component phase lives in that README and the roadmap (`docs/plan.md` §11). Do not scaffold a component ahead of its phase. "Empty" is not uniform: the API is provisional (plan §11 Wave 0; the decision record lives in docs/history/decisions.md), so drafting its skeleton in `api/` as a pre-1A design artifact is in scope, not a scope violation; `app/` starts with Phase 1A's minimal clients; the rest come in Phase 1A or later per their READMEs.

CLAUDE.md is a pointer that imports this file; edit AGENTS.md, never the pointer.

<!-- agents-md:managed:devlog -->

## Decision notes (devlog)

`devlog/` holds selective decision records, not session logs: at most
one note per work unit or PR in the ordinary case, named
`YYYY-MM-DD-HHMM-slug.md`. `devlog/README.md` is the protocol; most
work needs no note.

- **Write or update a note only when** the work involves at least one
  of: a consequential, non-obvious decision that rejects a plausible
  alternative; an investigation or verification result that materially
  changes the model, policy, risk, or implementation direction; a
  durable owner choice that would otherwise exist only in chat;
  cross-session context the work unit's PR or issue genuinely doesn't
  carry; or a change on the project's mandatory-note list, where it
  keeps one. Routine implementation, formatting, ordinary docs,
  dependency maintenance, mechanical syncs, and uncomplicated fixes
  need no note unless they reveal something consequential.
- **Content**: final rationale, rejected alternatives, changed
  assumptions, significant verification findings, and a "Revisit
  when ..." condition where one is useful; not commit diffs, test
  transcripts, or PR status. A note may evolve while its work unit or
  PR is active; it freezes on merge.
- **Retrieval**: read the notes linked from the issue or PR at hand;
  otherwise search by affected path, topic, contract, or decision
  name. Read the latest note only when resuming the work unit it
  describes. Prior notes are evidence, not prohibitions: do not
  silently overturn an explicit owner decision; if new evidence
  conflicts with one, identify the prior decision, state which
  assumption or condition changed, and surface the proposed revision.
- **Actionable deferred work goes to the issue tracker**, not the
  note. When an issue originates from a note, link the note from the
  issue; the note may carry a plain historical `Follow-up: #N` link,
  never a second source of status. An observation that is not yet
  actionable becomes a "Revisit when ..." statement, not open work.

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

## Default agent finish line

For any request to change code, docs, assets, or project state, the
default endpoint is **an open, review-ready PR with required checks
green**, not a merged branch. Merging is a human decision; do not merge
your own PR unless the user explicitly asks, or the project has adopted
an opt-in self-merge workflow.

Before implementation, establish a lightweight work contract: objective,
testable acceptance criteria, scope, dependencies and blockers, and explicit
non-goals. Direct user-assigned work needs no issue; the prompt and PR
carry the contract. Persist it in a tracker issue when the
work must survive a session boundary, pass sequentially between agents or
sessions (even within one short session), coordinate concurrent workers, or
join a backlog; a sequential handoff puts the durable input and output in
the issue and its comments, never only in transient chat. Actionable work
deferred out of the unit's scope gets a tracker issue before handoff.

A project may declare optional work-unit stages in an unmanaged,
project-specific section. While a declared stage is active, its recorded
allowed mutations and finish line govern: an implementation stage runs
only the checklist steps they permit and stops at its recorded
transition, a non-implementation stage follows its own record instead,
and completing a stage hands off to the next without authorizing it to
begin. Work that is not a declared stage runs the checklist in full,
minus any action a separately declared stage owns.

By default, begin work only through explicit user assignment. An issue, label,
backlog entry, satisfied dependency, completed plan, or claim is not
authorization to select and start work. Agent self-selection requires an
explicit project-specific opt-in policy.

The implementation checklist:

1. Read the README and, when resuming a work unit, its issue or PR and any
   decision note they link. Resolve the default branch explicitly, update it
   from its remote, and start from that exact tip (see Branches; only a
   declared stacked PR starts elsewhere).
2. Create one correctly named branch from that tip in a dedicated worktree
   or equivalent isolated checkout (see Branches for the primary-checkout
   exception).
3. Make the scoped change, with the docs/tests/assets that keep it complete
   and, where the project keeps decision notes, a note when the work meets
   its triggers.
4. Run the relevant verification plus the standard lint/build/test checks;
   if any check cannot run, record the exact gap in the PR.
5. Commit one concern at a time with a body that says why.
6. Push, open the PR with the template, and remove sections that do not apply.
7. Hand off per "Handing off the PR" (under Pull requests); leave the PR
   open for a human to review and merge.

For changes on a **destructive path** (delete/cleanup), a
**credential-leak surface**, or a **returned-object-trust boundary**
(trusting fields of a value handed back by an external call or
deserializer), read `docs/agent-workflow.md` §refute-first before
committing and run the verification pass it describes; a docs typo or
an off-path refactor doesn't trigger it.

<!-- /agents-md:managed:finish-line -->

<!-- agents-md:managed:context -->

## Context discipline

The working context is finite, and everything held in it is re-sent
with every later tool call, so transient bulk pulled in early taxes
every step after it. Durable state belongs in files (the PR body, the
issue, a decision note where the project keeps one); keep the working
context to what the current step needs.

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
- **Prefer a fresh session over a bloated one.** The PR body (plus a
  decision note when one exists) carries the durable state, so at a
  natural boundary (a PR handed off, a review round closed, a new work
  unit) in a long session, suggest continuing in a fresh session
  seeded with the PR number rather than pushing on; the accumulated
  context adds little to the next unit and dominates its cost.

<!-- /agents-md:managed:context -->

<!-- agents-md:managed:communication -->

## Writing for humans

Humans scan rather than read: a fifth of the words, weighted toward
first lines and line-starts, about four open items in mind, rapid
tune-out of repeated warnings. Write every human-facing artifact
(handoff, PR body, issue, plan, review comment, question) for that
reader; never rely on them digging.

- **Bottom line first.** Open the artifact with its conclusion,
  decision, or ask, along with any assumption or caveat it stands or
  falls on; supporting material follows in descending importance. A
  reader who stops after the opening still acts correctly.
- **Front-load every unit.** The first words of a heading, bullet, or
  paragraph carry its information.
- **Layer, don't just shrink.** The artifact is also the durable
  record: the skim layer carries the decision, while evidence,
  alternatives, and detail live below it or in the linked note or
  issue, never cut to shorten the skim layer.
- **Few asks per round, with defaults.** Surface the questions that
  gate the work, about three at a time, each with a recommended answer
  and a one-line reason. Convert questions a sensible default settles
  into visible assumptions the reader can veto; queue the remaining
  gating questions for a later round rather than assuming through
  them.
- **Ration flags, and calibrate them.** Tag severity, flag what
  changes the reader's decision or how much to trust the result, and
  make rare critical warnings visually distinct; a page of routine
  hedges buries the one that matters.
- **Surface uncertainty; don't polish past it.** State what was not
  verified and where you are unsure, so the human's attention lands
  where checking is needed; fluent prose invites rubber-stamping.

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
| `scripts/`    | Bash / Go      | `bash -n scripts/*.sh app/scripts/*.sh`; `shellcheck scripts/*.sh app/scripts/*.sh`; `bash scripts/test-build-image-references.sh`; `bash scripts/test-check-commit-messages.sh`; `bash scripts/test-merge-result-audit.sh`; `bash scripts/test-check-agent-image.sh`; `bash scripts/test-install-mac-app.sh`; `bash scripts/test-install-ios-app.sh` (CI pins shellcheck in `.github/workflows/scripts-ci.yml`); `go -C scripts/trackercollect build ./...`; `go -C scripts/trackercollect test ./...`; `go -C scripts/trackercollect vet ./...`; `bash scripts/run-real-work.sh <spec> <policy> <publication>` is the §11 1A.2 real unattended run and needs Apple `container` plus the operator preconditions its header lists |

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
- **Status signals:** on a **clean pass** (no findings) it posts no review and reacts 👍 (`+1`, i.e. `THUMBS_UP`) on the PR description a few minutes after the triggering event; that reaction, dated after the trigger, is the completion signal a review-watch keys off. On a **findings pass** it posts a `COMMENTED` review whose inline comments are each tagged by priority badge (P1/P2/P3) and invite a 👍/👎 reaction.

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

All work lands through a PR. Resolve and freshly update the repository's
default branch (`main` below), then create each ordinary work-unit branch
explicitly from that tip, never from the currently checked-out feature
branch; a non-default starting point is allowed only for an intentionally
declared stacked PR. Do the work as atomic commits (see Commits), then open
a PR; it merges with a real merge commit on a human's call. Never commit
directly to `main`, with no triviality exception: every bypass erodes the
`--first-parent` narrative.

Name branches `<type>/<short-kebab-slug>`: type from the Conventional
Commits vocabulary (`feat`, `fix`, `refactor`, `docs`, `chore`), slug
2–4 kebab-case words naming the work unit:

```text
feat/worksheet-promotion
fix/pane-focus-race
chore/swift-format-sweep
```

Exactly one slash (`feat/x` and a bare `feat` can't coexist). No ticket
numbers, dates, or owner prefixes; prepend an owner segment
(`bnw/feat/…`) only if multiple people or agents start pushing in
parallel. Merged branches auto-delete where that repo setting is on;
delete them after merge where it isn't.

**Break down concurrency before isolating it.** Keep coupled work in one work
unit, an explicit dependency chain, or an intentionally declared stack; a
worktree separates checkouts but cannot make logically dependent work safe in
parallel. Before substantive work, an assigned concurrent unit uses the
project's forge-visible claim mechanism, when one is defined. The claim
advertises active occupancy, not authorization; its form is project-specific.

**Isolate every implementation work unit** in a dedicated worktree or
equivalent isolated checkout. Where your platform and session support a
second checkout (a native worktree tool or session flag, or plain
`git worktree add <path> -b <type>/<slug> <default-branch>`), create the
branch and checkout from the freshly updated default-branch tip. Use the
primary checkout only when an explicit user or project instruction requires
it, or when the platform cannot create another checkout (no multi-checkout
support, or a sandbox pinned to one directory); then serialize all work on
one correctly based branch there and report the exception, never running
concurrent work units in one checkout. Remove a worktree once its branch
merges, standing outside the one being removed (`git worktree remove <path>`).

Work that depends on an open PR can stack on its branch instead of
waiting; see Stacked PRs under Pull requests.

<!-- /agents-md:managed:branches -->

<!-- agents-md:managed:pull-requests -->

## Pull requests

A PR is one work unit, reviewed as a whole and merged with a real merge
commit. Commits carry the atomic why (see Commits); the PR carries the
arc.

- **Title**: imperative, ≤ 72 chars, names the outcome, no type prefix
  or ticket noise ("Fix missing menu bar on unbundled launch"). In the
  intended repo setup the title (plus its number) is the _entire_ merge
  commit message; write it for `git log --first-parent` either way.
- **Body**: scaffolded by the repo's PR template
  (`.github/pull_request_template.md` on GitHub): Why, What (outcome bullets and a
  commit map keyed by subject, not SHA), Screenshots (UI changes only),
  Review Notes (optional), and Verification (bullets starting `Passed:`,
  `Checked:`, `Attempted:`, or `Not run:`; facts only). Before writing
  or updating the body, read `docs/agent-workflow.md` §pr-body and meet
  each section's bar (for UI changes, the Screenshots bar).
- **Self-review the diff in the PR files view before handing off**: the
  whole change as one artifact shows stray hunks, leftover debug code,
  scope creep, and accidental files. This is _mechanical hygiene_, not
  substantive critique.
- **Integration evidence belongs to one base commit.** CI results, a
  full-diff self-review, and a ready-for-handoff claim are valid only for
  the base commit they were checked against; a base-branch change
  invalidates all three, however clean the earlier diff looked.
- **Substantive critique needs fresh, ideally non-self eyes**, since
  same-context self-review shares the blind spots that produced the
  code: self-in-context < same-model fresh-context subagent <
  different-vendor bot / human. The bot reviewer or human is the
  load-bearing pass. For a non-trivial change, or a repo without a bot
  reviewer, read `docs/agent-workflow.md` §pre-push-review before
  pushing and run the platform-gated review it describes.
- **Record a noticed automated reviewer.** On seeing a bot-authored
  review or reviewer status signal the project hasn't recorded, read
  `docs/agent-workflow.md` §reviewer-record and add or augment the
  record before handing off.
- **Responding to automated review.** Evaluate each comment on its merits:
  fix real findings; push back, _with a one-line reason_, on contrived,
  speculative, or already-fixed ones; never reflexively comply. Reply
  inline with the disposition and the fixing commit SHA ("Fixed in
  `<sha>`" / a reasoned decline), then resolve the thread. Where fixes
  fold into their commits, fold all of a round's fixes and push once
  before any reply (the fold-then-reply gate in Commits), so every cited
  SHA is the final, pushed one. Resolving every thread is _not_ a hard
  merge gate; evaluate-on-merits is.
- **Fix the class, not just the cited line.** When a finding names one
  location, sweep the file and repo mechanically (grep for the finding's
  pattern, don't just eyeball nearby lines) and fix every instance in the
  same push; the class recurs in sibling sentences and files the citation
  never named. For validation or parsing code the sweep is the
  adversarial input-space enumeration in `docs/agent-workflow.md`
  §review-convergence; read it before widening the cited pattern.
- **Converge on a bar that rises with the rounds.** Blocking findings
  (correctness, security, data-loss, broken invariants, red CI) always
  earn another round; judge that severity yourself, the reviewer's tag
  being input, not verdict, and when unsure treat a finding as blocking.
  Once an exchange passes its early rounds or a finding recurs, read
  `docs/agent-workflow.md` §review-convergence before deciding on
  another. Hand off with every finding dispositioned (fixed, declined,
  deferred, or explicitly outstanding).
- **Keep the body current as review evolves the PR.** The body is the
  work unit's durable record on the forge: when review adds commits or
  shifts scope, update What, the
  commit map (flagging which commits resolve review findings, by
  subject), and Verification before re-handing-off. The inline reply on
  each resolved thread is the per-finding record; don't duplicate it
  into a standing "feedback" section.
- The intended repo settings enforce the Commits rules: merge commits
  only (squash and rebase disabled), title-only merge messages, and
  auto-delete of merged branches. Don't re-enable around them; where
  they aren't set, hold the same rules manually.

### Handing off the PR

Done means open, green, threads handled, self-reviewed, and no new
review activity outstanding. Once the PR is up, read
`docs/agent-workflow.md` §handing-off and follow its sequence:
review-watch per PR/reviewer first, anchored to the open or push event;
base-freshness pass with the base commit recorded; required checks
waited out, never a known-red handoff; self-review; watch closed out
with findings addressed or the bounded timeout recorded; then stop and
summarize.

If the user does ask you to merge, read `docs/agent-workflow.md`
§merge-and-resync before the merge or resync and follow it step by step;
do not merge or resync from memory.

### Reviewing a PR

When asked to review a PR, read `docs/agent-workflow.md` §reviewing-a-pr
first and hold its bar.

### Stacked PRs

Before creating a branch or PR that depends on an open PR, read
`docs/agent-workflow.md` §stacked-prs and declare the base explicitly,
never the current checkout.

<!-- /agents-md:managed:pull-requests -->

<!-- agents-md:managed:commits -->

## Commits

History serves three uses: diagnostics (blame/bisect lead to a
cause), reviewability (a PR reads commit-by-commit), and learning (the
log tells the project's evolution). Rules:

- **One concern per commit, every commit green.** If the body wants
  labeled sections (Correctness:/Performance:/…), it's more than one
  commit; split it. Each commit must build and pass tests on its own;
  never leave red intermediate states (it breaks bisect).
- **Body says why, not just what.** Write dense, specific bodies,
  wrapped ≤ 72 columns, referencing the work unit's decision note when
  one exists. State change deltas ("27 → 36 tests") if meaningful, never
  absolute status ("36 tests green"), which goes stale.
- **Never commit secrets** (credentials, tokens, keys, `.env`
  contents); reference them by name and use placeholders in examples.
- **Mechanical churn commits alone.** Reformats, renames, and moves get
  their own commit, added to `.git-blame-ignore-revs` in the same change
  (activate locally with
  `git config blame.ignoreRevsFile .git-blame-ignore-revs`).
- **Fold review fixes into the commit they belong to.** A fix that
  review or self-review turns up for an already-pushed commit folds into
  that commit, never an appended "address review" commit, keeping the
  merged PR clean and bisectable.
  Guardrails: every commit still builds and passes tests after the fold;
  `--force-with-lease`, **feature branch only, never force-push `main`**;
  only while the PR is unmerged (once merged, a fix is a new commit);
  update the matching decision note, when one exists, in the same
  operation. The mechanism (reset/amend/rebase) is your judgement. The
  fold-then-reply order is a gate: fold and push before writing the
  inline reply to the review thread, so the reply cites the final
  commit SHA, verified reachable from the pushed head; a standalone
  review-fix commit still on the branch at handoff is an unfinished
  fold, not a done round.
- **Never squash-merge multi-commit work**: it destroys the atomic
  structure above. A real merge commit keeps `git log --first-parent` as
  the work-unit narrative and the full log as the atoms; narrative
  subjects ("Walking skeleton: end-to-end flow") belong at that merge/PR
  level.

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

## Definition of done for an increment

Each increment is something actively used by the end of the work session:
not "code complete" or "tests pass" alone, but running and exercised.
Before calling work done:

The build succeeds, tests pass, and lint and formatting are clean.

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
  `bash scripts/test-install-mac-app.sh`, and
  `bash scripts/test-install-ios-app.sh` also pass
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
