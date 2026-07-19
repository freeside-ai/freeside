# AGENTS.md

**Freeside** is an agent control plane: a local, durable workflow controller that turns a software work item into an evidence-backed pull request and interrupts a human only when judgment is required. The spec, architecture, and roadmap live in [`docs/plan.md`](docs/plan.md); read it first, and argue changes against it. This file holds the development conventions that apply to every session: decision notes, branch/PR/commit discipline, and the monorepo's scope rules.

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

Agent-setup profile: High-assurance. A decision note is mandatory for:

- contract and safety-policy changes;
- material plan, architecture, or ADR decisions;
- destructive, credential-leak, or returned-object trust-boundary work;
- adversarial audits whose findings change policy or implementation;
- explicit owner choices that would otherwise exist only in chat.

Routine implementation and coordination require no note. GitHub issues
and git remain the only sources of active work state; a note records
why, never status.

<!-- agents-md:managed:finish-line -->

## Default agent finish line

For any user request that asks you to change code, docs, assets, or project
state, the default endpoint is **an open, review-ready PR with required
checks green**, not a merged branch. Merging is a human decision; do not
merge your own PR unless the user explicitly asks, or the project has adopted
an opt-in self-merge workflow.

Before implementation, establish a lightweight work contract: objective,
testable acceptance criteria, scope, dependencies and blockers, and explicit
non-goals. Direct user-assigned work needs no issue; the prompt and
eventual PR may carry the contract together. Persist that same contract
in a tracker issue when the work must survive a session boundary,
coordinate concurrent workers, or join a backlog. Actionable work
deferred out of the unit's scope gets a tracker issue before handoff.

By default, begin work only through explicit user assignment. An issue, label,
backlog entry, satisfied dependency, or claim is not authorization to select
and start work. Agent self-selection requires an explicit project-specific
opt-in policy.

Use this checklist for each work session:

1. Read the README and, when resuming an existing work unit, its issue or
   PR and any decision note it links. Resolve the repository's
   default branch explicitly, update it from its remote, and start ordinary
   work from that exact tip, not from whichever branch is currently checked
   out. Only an intentionally declared stacked PR may start from another open
   PR's branch (see Stacked PRs under Pull requests).
2. Create one correctly named branch explicitly from that starting tip.
3. Make the scoped change, including the docs/tests/assets that keep it
   complete and, where the project keeps decision notes, a note when
   the work meets its triggers.
4. Run the relevant verification plus the standard lint/build/test checks
   before PR; if any check cannot run, record the exact gap in the PR.
5. Commit one concern at a time with a body that says why.
6. Push, open the PR with the template, and remove sections that do not apply.
7. Hand off per "Handing off the PR" (under Pull requests): start the
   review-watch, complete the base-freshness pass, wait out required checks,
   handle reviewer activity, self-review the PR files view, and leave the PR
   open for a human to review and merge.

For changes on a **destructive path** (delete/cleanup), a
**credential-leak surface**, or a **returned-object-trust boundary**
(trusting fields of a value handed back by an external call or
deserializer), add a refute-first verification pass before committing
(independent lenses whose job is to _disprove_ the fix) and record
which findings were confirmed, rejected-by-verification (so they're
not re-raised), and accepted-by-decision: in the work unit's decision
note where the project keeps one, otherwise in the PR or issue. For a
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

## Build, test, run

The daemon (Wave 0 unit 1) and the API spec (Wave 0 unit 5) are initialized; the monorepo's other components are not. Per-component build, test, and run commands land in this table with each component's first PR (see `docs/plan.md` §11).

| Component     | Toolchain      | Commands                                      |
| ------------- | -------------- | --------------------------------------------- |
| `daemon/`     | Go             | `cd daemon`; `go build ./...`; `go test ./...`; `go vet ./...`; `golangci-lint run` |
| `app/`        | Xcode / SPM    | `cd app`; `./scripts/generate-api-client.sh`; `swift test`; `xcodebuild -project Freeside.xcodeproj -scheme FreesideMac -destination 'platform=macOS' -skipPackagePluginValidation CODE_SIGNING_ALLOWED=NO build`; `xcodebuild -project Freeside.xcodeproj -scheme FreesideIOS -destination 'generic/platform=iOS Simulator' -skipPackagePluginValidation CODE_SIGNING_ALLOWED=NO build`; `bash scripts/run-convergence.sh` (repo root; §5.14 real-daemon convergence, builds the daemon harness) |
| `api/`        | OpenAPI (spec) | `go run github.com/daveshanley/vacuum@v0.29.9 lint -r api/vacuum.ruleset.yaml --details --fail-severity warn api/openapi.yaml` (from repo root; see api/README.md) |
| `prompts/`    | prompt text    | not yet initialized; see docs/plan.md roadmap |
| `policy/`     | YAML (policy)  | not yet initialized; see docs/plan.md roadmap |
| `images/`     | OCI images     | `bash scripts/build-exporter-image.sh` (builds `images/exporter/`; needs Apple `container` or `docker`); agent bases not yet initialized |
| `scripts/`    | Bash           | `bash -n scripts/*.sh`; `shellcheck scripts/*.sh`; `bash scripts/test-merge-result-audit.sh` (CI pins shellcheck in `.github/workflows/scripts-ci.yml`) |

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

## Automated reviewer

**Codex** reviews pull requests automatically. Respond to its findings per **Responding to automated review** under Pull requests, and filter later review activity by its login.

- **Login/account:** `chatgpt-codex-connector` (the `chatgpt-codex-connector[bot]` form appears on inline review comments and in the pulls review-comments API).
- **Triggered:** automatically on PR open-for-review, mark-ready, and each push (it re-reviewed after every push this session); also on demand via an `@codex review` comment.
- **Status signals:** on a **clean pass** (no findings) it posts no review and reacts 👍 (`+1`, i.e. `THUMBS_UP`) on the PR description a few minutes after the triggering event; that reaction, dated after the trigger, is the completion signal a review-watch keys off. On a **findings pass** it posts a `COMMENTED` review whose inline comments are each tagged by priority badge (P1/P2/P3) and invite a 👍/👎 reaction.

## Integration ordering and merge-result audit

Freeside's mechanical defense for the integration-evidence invariant
(**Integration evidence belongs to one base commit**, under Pull
requests): a branch carrying stale or inverse content can silently
revert already-merged sibling work through a clean 3-way merge (#47
reverting #48; recovered in #49). The audit constructs the prospective
merge result against the current base tip without mutating the
checkout and enforces the unit's declared path scope on it.

- The spine role owns final integration ordering when multiple PRs are
  ready; a work unit's Dependencies field encodes required
  serialization and intentional stacks (see Stacked PRs).
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
explicitly from that tip. Never create an ordinary branch from the currently
checked-out feature branch; a non-default starting point is allowed only for
an intentionally declared stacked PR. Do the work as atomic commits (see
Commits), then open a PR; the work merges with a real merge commit, a human's
call per the finish line. Never commit directly to `main`. No triviality
exception: every bypass erodes the `--first-parent` narrative.

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

**Break down concurrency before isolating it.** Keep coupled work in one work
unit, an explicit dependency chain, or an intentionally declared stack; a
worktree separates checkouts but cannot make logically dependent work safe in
parallel. Before substantive work, an assigned concurrent unit uses the
project's forge-visible claim mechanism, when one is defined. The claim
advertises active occupancy, not authorization; its form is project-specific.

**Isolate concurrent work units.** Concurrent work units must use separate
worktrees or checkouts. Where your platform and session support a second
checkout (a native worktree tool or session flag, or plain
`git worktree add <path> -b <type>/<slug> <default-branch>`), create each
worktree explicitly from the freshly updated default-branch tip, not from
whatever branch is checked out; prefer the same isolation for a single work
unit. Remove the worktree once its branch merges
(`git worktree remove <path>`). Where isolated checkouts are unavailable
(no multi-checkout support, or a sandbox pinned to one directory), serialize
the work units and use one correctly based branch at a time in the primary
checkout. Never run concurrent work units in one checkout.

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
    motivation. Link the decision note when one exists; don't duplicate it.
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
    alternatives live in the decision note when they do.
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
- **Integration evidence belongs to one base commit.** CI results, a
  full-diff self-review, and a ready-for-handoff claim are valid only for the
  base commit they were checked against. A base-branch change invalidates all
  three, even when the earlier PR diff looked clean.
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
- **Validate against the current base before final handoff.** Resolve the
  current base tip, update the PR branch using the project's merge or rebase
  convention, rerun the relevant verification, and self-review the complete
  refreshed diff. Record the base commit used for that final validation in
  the PR's Verification section or the handoff. If the base advances again
  after handoff but before merge, the PR is stale and needs another
  integration pass. If you do not own the branch or lack permission to
  update it, report the stale state instead of silently rewriting it.
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
  any linked decision note; check the change does what it claims, that
  Verification matches reality, and that docs/tests moved with behavior.
  Recorded decisions are evidence, not prohibitions: don't silently
  overturn an explicit owner decision; if the diff conflicts with one,
  name the decision and which assumption or condition changed.
- **Stay in scope.** Out-of-scope improvements are non-blocking nits or a
  follow-up issue, not merge-blockers; don't grow the PR through review.
- **Scale depth to risk.** Routine PRs get a normal pass; destructive /
  credential-leak / trust-boundary changes get the refute-first lens (see the
  finish line). A docs typo doesn't.
- **Resolve explicitly.** State what would unblock; let the author
  fix-or-decline. Resolving every thread isn't the gate; agreement on
  blockers is.

### Stacked PRs

Dependent docs or cleanup work can proceed without waiting for its base as an
intentionally declared stacked PR. A non-default base is an explicit
dependency: name the open PR's branch when creating both the follow-up branch
or worktree and the PR, never inherit it from the current checkout. On GitHub,
use `gh pr create --base <feature-branch>`; it auto-retargets to `main` when
the base merges, while other forges may require manual retargeting. Two
gotchas: while the base is open the stacked PR's diff shows only its own
commits; and if the base is force-pushed (the fold-review-fixes rule in
Commits), `rebase --onto` the stack onto the new base tip.

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
  wrapped ≤ 72 columns. Reference the work unit's decision note
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
  update the matching decision note, when one exists, in the same
  operation. The mechanism (reset/amend/rebase) is your judgement.
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
- Merge-result audit run against freshly fetched `origin/main` before
  handoff, base SHA and verdict recorded in PR Verification (see
  Integration ordering and merge-result audit); when `scripts/` is in
  scope, `bash scripts/test-merge-result-audit.sh` also passes
- Decision note written or updated when the work hits a Decision notes
  trigger or the mandatory-note list; most work needs none

<!-- /agents-md:project:done-checks -->

<!-- /agents-md:managed:done -->

## Coordination

Coordination state lives in GitHub and git, never in status files. Issues
persist every work unit that outlives a direct, session-contained
assignment; this section defines how to find, claim, and finish one.
Runtime AttentionItems (docs/plan.md §4) are a different system; this
section governs building Freeside, not running it.

### Work units

Every work unit carries the lightweight work contract the finish line
defines (objective, testable acceptance criteria, scope, dependencies and
blockers, explicit non-goals); this section governs where that contract
persists. A direct, session-contained user assignment may carry the
contract in the prompt and PR together. Scheduled work,
backlog work, work that spans sessions, and work involving more than one
agent require a work-unit issue; when a direct task crosses one of those
boundaries mid-flight, promote it to an issue before continuing.
Scheduled self-selection (the scheduling door under Pickup) remains this
project's explicit self-selection opt-in, unchanged by the persistence
rule.

One issue per issue-backed work unit, created from the work-unit
template: Source devlog entry (optional; cite the originating decision
note's filename only when the issue genuinely originated in one),
Objective, Non-goals (`none` allowed), Affected
interfaces/contracts (the interface surfaces the unit touches, not the
whole work contract; the issue as a whole is the contract), Acceptance
(the fixture/test list is the spec), Scope / declared paths, Dependencies
(blockers, required serialization, intentional stacked bases, and
integration order, not only issue refs).
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

A claim records occupancy only; authorization comes from scheduling or
fiat (see Pickup), never from the claim itself. Issue-backed work is
claimed with an issue-comment lease that hands off to a real PR. Direct
no-issue work needs no claim: it is not eligible for concurrent or
multi-session execution, and gets promoted to an issue before that
changes (see Work units).

To claim a unit:

1. Confirm the issue is authorized (scheduled or fiat-assigned) and has
   no active claim: a full paginated read of its comments plus the
   open-PR check below.
2. Choose the branch name (per the Branches section) and post a claim
   comment on the issue: the versioned marker line plus one visible
   `Claim:` line naming that branch.

   ```text
   <!-- freeside-work-claim:v1 -->
   Claim: feat/example-slug
   ```

3. Re-read all of the issue's comments with pagination. Among
   non-expired, unreleased claim comments, the earliest `created_at`
   wins; the numeric comment ID is the deterministic tie-breaker (lower
   wins). Ordering is by creation time; comment edits do not reorder
   claims.
4. A losing claimant posts a release comment bound to its own claim and
   stops (it may re-claim later with a new comment). A release comment
   releases exactly the claim comment whose numeric ID its
   `Releases-claim:` line names, never other claims: branch names do not
   identify a claim, since concurrent claimants following the same slug
   convention can choose the same one. The `Release:` line repeats the
   branch for human readability only.

   ```text
   <!-- freeside-work-release:v1 -->
   Release: feat/example-slug
   Releases-claim: 1234567890
   ```

5. The winner creates its dedicated worktree/branch from the freshly
   updated default-branch tip (per Branches) and begins work. No empty
   claim commit: the branch's first commit is real work.

The lease expires 48 hours after the claim comment's creation if no open
PR from the claimed branch carries the issue's close keyword by then; an
expired lease is dead, and re-claiming needs a new comment. Once an open
PR from the same branch contains the close keyword, that PR is the
active claim and the comment lease is subsumed (no further expiry).
Closing that PR unmerged releases the claim; merging closes the issue
normally.

The active claim for a unit is therefore: a non-expired, unreleased
comment lease; or an open PR from the lease's branch with the issue's
close keyword; or, during the transition from the previous protocol, a
legacy open PR claiming the unit with a `Claim #N` commit or close
keyword. A bare cross-reference (`Refs #N`) is never a claim. One claim
per unit: if an active claim exists, pick another unit. Do not create
new empty claim commits; drop any legacy one in the next branch rewrite
(the fold-fix rules under Commits). Claim state is verified, never
assumed: a comment or PR API read or write failure at any step fails
closed, and work does not begin (or continue past the failed step) while
claim state cannot be verified. Collaborator comments are trusted;
adversarial comment editing is outside this protocol's threat model.

`needs-human` deferrals use the fiat door defined under Deferral escalation,
never self-selection: after the maintainer acts, fiat assigns the issue to a
session; the session verifies the external state and records the audit
diff in the ordinary close-keyword PR, adding a decision note only when
the outcome hits a Decision notes trigger or the mandatory-note list.

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
   unit's Affected interfaces/contracts field cites.
2. When resuming an existing unit, read its issue or PR and any decision
   note it links (Decision notes section).
3. Status queries:
   - open PRs and their declared paths: overlap with yours means stop and
     coordinate via issue comment before claiming;
   - active claims on any unit you intend to claim: the paginated
     comment-lease read plus open-PR check under Claiming;
   - the current wave's pinned tracking issue;
   - open `kind:contract` issues, ignoring a `deferral` issue until it is
     scheduled or has an active claim, then excluding the unit you are claiming
     and any unit whose Dependencies chain includes it (a
     dependency-ordered chain of contract units keeps at most one
     claimable at a time, so downstream chain members may stay filed
     without blocking their chain head): among the remainder, if one
     touches your Affected interfaces/contracts, block on it; when claiming a
     `kind:contract` unit, block on every other remaining open contract unit
     (contract work is serialized).
4. Verify each dependency's PR is merged.

### Session end

Write or update the unit's decision note only when a Decision notes
trigger or the mandatory-note list applies. Additionally: deferrals
discovered mid-unit follow Deferral escalation below; tick your unit on
the wave tracking issue when your PR merges (or note partial state on
the issue).

### Deferral escalation

Actionable work deferred out of a unit's scope gets a tracker issue
before handoff (per the finish line); the escalation follows these
rules:

- **Provenance when a note exists**: the issue form's optional
  `Source devlog entry` field cites the originating decision note's
  filename; the note may carry a plain `Follow-up: #N` historical link.
  Most escalations originate in the work itself and leave the field
  blank. Historical entries are frozen: never write markers or other
  mutations back to them.
- **Lane label routes by owner, not discoverer**: the lane whose Scope /
  declared paths contain the work. Shared-package needs use
  `kind:contract` plus the **`deferral`** origin label.
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
- Closure is ordinary: a work-unit PR with a close keyword; the issue
  carries the item's whole status lifecycle.

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
