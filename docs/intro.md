# An Introduction to Freeside

This is the plain-language companion to the project: the goals and the core
ideas, without the plan's implementation detail. It is **not normative**: it
sets no rules and settles no disputes. It describes the product as intended.
The workflow below now runs end to end on a real repository; the status pointer
under "Where To Go Next" says what has landed and what is still being built.

The source of truth is [`docs/plan.md`](plan.md). Where the two disagree, the
plan wins, and nothing should cite this document as authority.

## The Problem

Coding agents have gotten good at the work itself. Give one a well-specified
task and a harness like Claude Code or Codex (the tool the agent runs inside),
and the agent will often produce a reasonable result. The agent still needs a
human to guide and check its work, and as agents get better at execution, human
attention becomes the scarce resource.

The goal is a positive return: work that is useful and correct, and worth more
than it costs in your attention, maintenance, money, and risk. Autonomy helps
only if it improves that return.

Both sides of that balance can be engineered. An agent delivers more when it
can run for long stretches without stopping to ask permission. You contribute
more when your attention goes to what only you can supply: judgment, taste, and
a wider view of the project's goals and trade-offs. Oversight is part of that
contribution. It catches failures early, so it can't be optional. It also has
to be easy, because oversight that feels like a chore gets skipped.

Giving agents that much autonomy is risky, though. The agent runs unattended on
a machine that may also hold your personal files and logged-in accounts, so the
setup needs serious safeguards. The work also has to be verifiable: before a
result ships, you need something stronger than the agent's word that the work
is done.

Harnesses solve the inner loop: the model, the tools, the edit-test cycle.
Freeside is the outer loop. It decides what work starts and inside what
boundary, which harness and model do it, what evidence "ready" requires, what
keeps moving while you are away, and which decisions come to you.

Freeside is **an agent control plane**: the layer that directs agents rather
than running them. It never runs the model loop itself. It launches and
supervises the harnesses that do: the harness runs the agent, and you hold the
reins.

## What Freeside Does

**Freeside is a local, durable workflow controller that grants agents the
autonomy to turn work items into evidence-backed pull requests and interrupts
you only when your judgment is required.**

Workflows, initiators (the rules that start work), and policy are written down,
explicit, and versioned. Permission moves out of moment-to-moment interruptions
and into deliberate, standing grants.

The intended experience, end to end:

1. A work item arrives. You submitted it, labeled an issue, or a scanner
   proposed it.
2. A specifier (a planning pass that researches the item) turns it into a
   specification.
3. The specification reaches your attention inbox, and you approve it.
4. An agent implements it in an isolated workspace, where the agent can work
   at full speed without stopping for permission.
5. When the agent exits, its output crosses a hostile import boundary (an
   importer that treats everything it receives as untrusted) into a fresh
   checkout.
6. Freeside runs approved checks against the result and captures evidence
   in a clean environment.
7. An independent reviewer reads the candidate. Each finding ends in one
   final disposition (fixed, declined, or deferred; fixed means a re-review
   showed it gone), and remediation and re-review continue while they still
   pay off. Round counts are emergency brakes, not the normal stopping rule.
8. The daemon publishes the verified, reviewed candidate as a pull request
   under an audited trust profile (a reviewed record of what the
   repository's own automation may do). The PR carries the review's
   disposition history: what happened to each finding, and why.
9. A ready-for-review card reaches your phone. It carries mechanical evidence
   alongside an agent's assessment, each labeled for what it is.
10. You review and merge on GitHub.

GitHub stays the system of record for code, reviews, and merging. Freeside runs
the workflow and decides when to involve you. Merging stays yours.

The model, the harness, and the reasoning budget (how much thinking a run may
spend) are choices Freeside can route. Freeside sits above the harness, so
routing policy lives there, informed by task class, quality, latency, usage,
and cost.

## The Core Ideas

### Interruptions Are a Product, Not a Side Effect

Most automation treats interrupting you as an afterthought. Freeside treats
interruptions as a first-class product. An interruption is a durable record
with a lifecycle and a self-contained decision card that carries the context
you need to decide. Approvals bind to the exact versions you approved; if an
input changes, the approval no longer holds. A card may lead with a recommended
action, labeled with its source. How often you override the recommendation
measures how good the recommendations are; the override rate is never a target.
Every interruption is classified as a planned gate (a stop the workflow
expects) or an exception. A rising exception rate is a defect to fix, and a
kind of interruption that keeps recurring is a candidate for a policy change
that removes it.

Presentation is part of the same design. Agents produce more text than anyone
will read, and unread text pushes you out of the loop. So a decision card leads
with the ask and a summary you can absorb in seconds, with detail pushed lower.
Changes arrive summarized, plans lead with the open questions, and feedback
comes digested.

A summary is itself something you must decide whether to trust. So each names
its producer, preserves uncertainty and dissent, and links to the evidence it
compresses. The measure of an attention item is whether you can understand it
and make an informed decision quickly.

Freeside calls the interruption service the **signet**.

### Autonomy Is Bounded, and the Boundary Does the Watching

An agent that must ask before every consequential step gives you barely more
leverage than doing the work yourself. Freeside constrains the boundary instead
of watching the agent. Publication authority stays outside: the workspace holds
no GitHub credentials, so nothing inside it can push to or publish on GitHub.
Capabilities are fixed when the run starts, and every stage runs under a named
network-access profile (an egress profile) that states plainly how much risk
that access carries.

Some risk remains: provider credentials, permitted network paths, and resource
consumption. Freeside bounds these where it can, monitors them, and names them
explicitly.

When an agent hits a wall, such as a missing capability or a question only you
can answer, it cannot escalate on its own. The run pauses and creates an
attention item, and you decide: retry with a larger pre-approved capability
set, answer the question, or stop.

The boundary's central proof is the workspace handoff. Before anything leaves,
the context that held credentials is terminated, and the workspace is remounted
read-only in a fresh context that never had them. A runtime that cannot prove
that sequence is not trusted to have performed it. No environment variable,
cache, or process memory can leak from the surrounding environment, because the
exporting context never held any of them. What the handoff cannot guard against
is a secret written into the workspace files themselves. So exports are scanned
for secrets afterwards, and that scan is best-effort by nature: it reduces
risk, but it cannot prove absence.

The environment that enforces this is the **ward**.

### Agent Output Is Untrusted; Verification Is Independent

An agent's workspace is a working copy, but an untrusted one. Exactly two
channels leave it: the repository changes, meaning the changed file contents
with a manifest (a list of what changed) in a fixed form, and evidence
artifacts of declared kinds. The export may also include a proposed commit
plan: how the changes group into commits, in what order, with what messages.
The plan rides the repository-change channel as bounded, untrusted data.
Everything git-shaped stays behind and is never read by anything trusted: the
`.git` directory, hooks, objects, and the agent's own commit history. The plan
remains hostile input. Its messages are places a credential could leak or
automation could be steered, but the importer can bound, decode, validate, and
screen it without interpreting Git. An out-of-process importer validates the
export. Freeside then re-authors clean commits onto the exact base it handed
the agent: one commit for each accepted group that still contains a change, or,
when no plan is accepted and policy permits the fallback, a single commit.

"Done" has a mechanical gate the implementer does not control. Verification
runs in a clean room with no credentials and no network. The checks load only
from approved configuration, never from the agent being judged. The clean room
proves the approved checks ran against the exact bytes and records enough about
where everything came from (its provenance) to run the verification again.

Verification cannot prove the checks are sufficient. Tests can be incomplete,
and a candidate's changes to its own tests are part of what is under review.
Verification decides whether work counts as ready; it raises confidence but
does not replace judgment. The verifier, not the agent, captures evidence. The
agent's assessment travels labeled as a claim, and every artifact records who
produced it and from what.

This whole path, from export to verified candidate, is the **gauntlet**.

**Publication has attack surfaces of its own. CI is the first.** A pull request
wakes the repository's own automation, and CI jobs carry real authority even
when their YAML names no secret. Every managed repository gets an audited
**automation trust profile** that describes what a pull-request job can
actually do. Freeside locks the profile to the reviewed version, stops opening
pull requests if the repository's automation changes without review, and keeps
unreviewed candidate code out of secret-bearing CI.

**Instructions are the second.** Prompts, policy, verification checks, and
files like `AGENTS.md` load only from an approved commit, never from an agent's
workspace. The one exception is the operator host's own vendor instruction
file, which is captured when the run is admitted and pinned by its hash. A
review is not independent when the thing under review picked its reviewer's
instructions. So the implementing agent and its reviewer both build their
instructions from the approved commit (the trusted base), and the candidate's
copy never governs its own review. A candidate that edits a
reviewer-instruction file such as `AGENTS.md` still publishes. The edit is
surfaced as an advisory finding in a PR-body section the candidate's prose
cannot forge; you judge it at merge, and the edit governs only later runs.
Changes to CI and other automation-control paths still block publication.
Guarded is not frozen: what agents learn flows back into prompts and policy
through the same reviewed, gated path as any other change.

### Decisions Are Durable

Unattended operation is only safe if a crash cannot lose a decision or double
an action. A decision, once committed, survives restart, no matter when the
process dies. External actions whose result can be checked afterwards are
retried until they match the decision; anything that cannot be safely retried
waits for you. Backup is a complete, encrypted checkpoint that restores a
coherent whole or refuses to restore at all, and unattended operation is gated
on backup health.

## What Freeside Is Not

- **Not a harness.** It never owns a model loop. It orchestrates harnesses
  and supported vendor interfaces.
- **Not an IDE or code-review surface.** Code reading, pull-request review,
  and merging stay on GitHub; Freeside owns workflow decisions and approvals.
  It does not merge for you. A human merge is an accountability checkpoint,
  and whether narrow, risk-bounded classes of change ever earn automatic
  merge is deliberately left open.
- **Not a multi-tenant product.** It is designed for one owner, not many
  users: no accounts, no billing.
- **Not self-modifying.** Changing the rules is itself gated work, and
  control-plane configuration never changes at runtime.

## How Success Is Measured

The project succeeds only if all four conditions hold, and each needs an
operational measure:

1. **Useful, correct work per unit of your attention rises** against a
   baseline adjusted for volume and risk.
2. **Decision quality is preserved**, checked by sampled audits and by
   tracking work marked ready that later fails review or verification.
3. **The safety invariants hold**, verified by conformance tests (do the
   rules hold?) and adversarial tests (can they be broken?) rather than read
   off telemetry.
4. **Autonomy stays real:** exceptional interruptions stay rare and trend
   down.

Freeside records the raw material from day one: interruption classes, how long
each item waits from delivery to decision, run outcomes, and cost. The baseline
is logged passively alongside normal work. Turning that into honest measures is
part of the work. Fast decisions do not prove low attention or good judgment,
and fewer interruptions can mean suppressed warnings. So decision quality needs
sampled audits, normalization by volume and risk, and an accounting of
maintenance.

These four are necessary gates. Cost and maintenance still decide whether
passing them is worth it.

## Where To Go Next

- [`docs/github-app-postures.md`](github-app-postures.md) explains the default
  public GitHub App posture (who owns the GitHub App identity Freeside acts
  under), its residual risks, and when the private work-account posture is
  the better fit.
- [`docs/plan.md`](plan.md) is the charter and specification. Sections 1–4
  define the product and attention model, 5 the architecture and its
  binding contracts, 6–10 verification through operations, 11–12 the
  roadmap and exit criteria, and 13–15 decisions, risks, and naming.
- [`docs/decisions/`](decisions/README.md) holds the architecture decision
  records, including the AGPL-3.0-or-later license and the advisory
  treatment of reviewer-instruction edits.
- [`AGENTS.md`](../AGENTS.md) holds the development conventions for this
  repository, and [`docs/coordination.md`](coordination.md) explains how
  parallel agent sessions claim and coordinate work.
- The [plan's implementation-coordination section](plan.md#implementation-coordination-building-freeside-with-agents)
  explains how to find the live phase, wave, and active implementation front
  (which units are being built now).
