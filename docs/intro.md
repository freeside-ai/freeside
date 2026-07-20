# An Introduction to Freeside

This is the narrative companion to the project: the goals and core ideas presented
in plain language, without the plan's implementation details. It is **not
normative**, and describes the intended product, not necessarily what exists today.

The source of truth is
[`docs/plan.md`](plan.md). Where the two disagree, the plan wins, and nothing
should cite this document as authority.

## The Problem

Coding agents have gotten good at the work itself. Give one a well-specified
task and a harness like Claude Code or Codex, and it will often produce a
reasonable result. It still needs human guidance and verification, however,
and as agent execution improves, human attention increasingly becomes the
limiting resource.

The goal is a positive return: work that's useful and correct and worth more
than the cost of your attention, maintenance, money, and risk. Autonomy helps
only if it improves that return.

Both sides of that balance can be engineered. An agent delivers more when it
can run long stretches without stopping for permission. You contribute more
when your attention goes to what only you can supply: judgment, taste, and a
wider view of the project's goals and trade-offs. Part of your contribution
is oversight: it's how failures are caught early, so it can't be optional.
The design goal is to make it frictionless, because oversight that's a chore
is oversight that gets skipped.

Giving agents the necessary autonomy is risky, though. The agent runs unattended
on a machine that may also hold your personal files and authenticated accounts,
which demands serious safeguards. The work also has to be verifiable: before a
result ships, you need something stronger than the agent's word that it is
done.

Harnesses solve the inner loop: the model, the tools, the edit-test cycle.
Freeside is the outer loop: what work starts and inside what boundary, which
harness and model do it, what evidence readiness requires, what keeps moving
while you are away, and which decisions come to you.

Freeside is **an agent control plane**. It never runs the model loop itself;
it launches and supervises the harnesses that do: the harness runs the
agent, and you hold the reins.

## What Freeside Does

**Freeside is a local, durable workflow controller that grants agents the
autonomy to turn work items into evidence-backed pull requests and interrupts
you only when your judgment is required.**

Workflows, initiators, and policy are explicit, versioned artifacts: permission
moves out of moment-to-moment interruptions and into deliberate, standing grants.

The intended experience, end to end:

1. A work item arrives: you submitted it, labeled an issue, or a scanner
   proposed it.
2. An elaboration step turns it into a specification.
3. The specification reaches your attention inbox, and you approve it.
4. An agent implements it in an isolated workspace, where it can work at
   full speed without stopping for permission.
5. When the agent exits, its output crosses a hostile import boundary into
   a fresh checkout.
6. Freeside runs approved checks against the result and captures evidence
   in a clean environment.
7. The daemon publishes the verified candidate as a pull request under an
   audited trust profile.
8. Automated review and remediation iterate within explicit limits.
9. A ready-for-review card reaches your phone, carrying mechanical evidence
   alongside an agent's assessment, each labeled for what it is.
10. You review and merge on GitHub.

GitHub stays the system of record for code, reviews, and merging. Freeside
orchestrates and decides when to involve you. Merging stays yours.

The model, harness, and reasoning budget are routable choices. Sitting above the
harness, Freeside is where routing policy lives, informed by task class, quality,
latency, usage, and cost.

## The Core Ideas

### Interruptions Are a Product, Not a Side Effect

Most automation treats interrupting you as an afterthought. Freeside treats
interruptions as a first-class product. An interruption is a durable record with
a lifecycle and a self-contained decision card carrying the context you need
to decide. Approvals bind to the exact versions approved; changed inputs
invalidate them. Every interruption is classified as a planned gate or an
exception. A rising exception rate is a defect to fix, and a recurring kind
of interruption is a candidate for a policy change that removes it.

Presentation is part of the same design. Agents produce more text than
anyone will read, and unread text pushes you out of the loop. So a decision
card leads with the ask and a summary you can absorb in seconds, with detail
pushed lower. Changes arrive summarized, plans lead with the open questions,
feedback comes digested.

Summaries are themselves a trust surface: each identifies its producer,
preserves uncertainty and dissent, and links to the evidence it compresses.
The measure of an attention item is whether or not you could understand it
and make an informed decision quickly.

Freeside calls the interruption service the **signet**.

### Autonomy Is Bounded, and the Boundary Does the Watching

An agent that must ask before every consequential step is providing barely
more leverage than doing the work yourself. Freeside constrains the boundary
instead of watching the agent. Publication authority stays outside: the
workspace holds no GitHub credentials, so nothing inside can push to or
publish on GitHub. Capabilities are fixed when the run starts, and every
stage runs under a named network-egress profile with an honest risk class.

Some risk remains: provider credentials, permitted network paths, and
resource consumption. Freeside bounds these where possible, monitors them,
and names them explicitly.

When an agent hits a wall, such as a missing capability or a question only
you can answer, it cannot escalate itself. The run pauses and creates an
attention item, and you decide: retry with a larger pre-approved capability
set, answer the question, or stop.

The boundary's central proof is the workspace handoff: before anything leaves,
the credential-bearing context is terminated and the workspace is remounted
read-only in a fresh context that never had credentials. A runtime that cannot prove that
sequence is not trusted to have performed it. Nothing ambient, no environment
variable, cache, or process memory, can leak, because the exporting context never
held it. What this handoff cannot guard against is secrets written into the
workspace files themselves, so exports are then scanned for secrets, best-effort
by nature: a scan reduces risk; it cannot prove absence.

The environment that enforces this is the **ward**.

### Agent Output Is Untrusted; Verification Is Independent

An agent's workspace is a working copy, but an untrusted one. Exactly two
things leave it: the changed file contents with a normalized manifest, and
typed evidence artifacts. The agent commits its work normally, and those
commit boundaries and messages may ride along as plain, validated data;
everything else, including the `.git` directory itself, its hooks, and its
objects, stays behind. The split is deliberate: hooks, configuration, and
hand-crafted git objects are the parts of a repository that can carry an
attack, while the commits' boundaries and messages are just data that can
be checked like any other. An out-of-process importer validates the export,
and Freeside re-authors clean commits of the validated contents onto the
exact base it handed the agent: one clean commit for each branch-line
agent commit that still changes something after normalization, when that
history rides along, and a single commit otherwise.

"Done" has a mechanical gate the implementer does not control. Verification
runs in a clean room with no credentials and no network, using checks loaded
only from approved configuration, never supplied by the agent being judged.
The clean room proves the approved checks ran against the exact bytes and
records enough provenance to rerun the verification.

It cannot prove the checks are sufficient: tests can be incomplete, and a
candidate's changes to its own tests are part of what is under review.
Verification gates readiness; it raises confidence but does not replace
judgment. The verifier, not the agent, captures evidence; the agent's
assessment travels labeled as a claim, and every artifact records who produced
it and from what.

This whole path, from export to verified candidate, is the **gauntlet**.

**Publication has attack surfaces of its own. CI is the first.** A pull request
wakes the repository's own automation, and CI jobs carry real authority even
when their YAML names no secret. Every managed repository gets an audited
**automation trust profile** describing what a pull-request job can actually do;
Freeside locks the profile to the reviewed version, stops opening pull requests
if it drifts, and keeps unreviewed candidate code from executing in
secret-bearing CI.

**Instructions are the second.** Prompts, policy, verification checks, and
files like `AGENTS.md` load only from an approved commit, never from an agent's
workspace, and a change that would edit the instructions governing its own
review is blocked: a review is not independent when the thing under review picked
its reviewer's instructions. Guarded is not frozen: what agents learn flows back
into prompts and policy through the same reviewed, gated path as any other
change.

### Decisions Are Durable

Unattended operation is only safe if a crash cannot lose a decision or double an
action. A decision, once committed, survives restart, no matter when the process
dies. Reconcilable external actions are retried until they converge; anything
that cannot be safely retried waits for you. Backup is a complete, encrypted
checkpoint that restores a coherent whole or refuses to restore, and unattended
operation is gated on backup health.

## What Freeside Is Not

- **Not a harness.** It never owns a model loop; it orchestrates harnesses
  and supported vendor interfaces.
- **Not an IDE or code-review surface.** Code reading, pull-request review,
  and merging stay on GitHub; Freeside owns workflow decisions and
  approvals. It does not merge for you: human merge is an
  accountability checkpoint. Whether narrow, risk-bounded classes of change
  ever earn automatic merge remains deliberately open.
- **Not a multi-tenant product.** Designed for one owner: no accounts, no
  billing.
- **Not self-modifying.** Changing the rules is itself gated work;
  control-plane configuration never changes at runtime.

## How Success Is Measured

The project succeeds only if all four conditions hold, and each needs an
operational measure:

1. **Useful, correct work per unit of your attention rises** against a
   normalized baseline.
2. **Decision quality is preserved**, checked by sampled audits and by
   tracking work marked ready that later fails review or verification.
3. **The safety invariants hold**, verified by conformance and adversarial
   tests rather than read off telemetry.
4. **Autonomy stays real:** exceptional interruptions stay rare and trend
   down.

Freeside records the raw material from day one: interruption classes,
delivery-to-decision timing, run outcomes, and cost. The baseline is logged
passively alongside normal work. Turning that into honest measures is
itself part of the work: fast decisions do not prove low attention or
good judgment, and fewer interruptions can mean suppressed warnings, so
decision quality needs sampled audits, normalization by volume and risk,
and an accounting of maintenance.

These four are necessary gates; cost and maintenance still determine
whether passing them is worth it.

## Where To Go Next

- [`docs/plan.md`](plan.md) is the charter and specification. Sections 1–4
  define the product and attention model, 5 the architecture and its
  binding contracts, 6–10 verification through operations, 11–12 the
  roadmap and exit criteria, and 13–15 decisions, risks, and naming.
- [`AGENTS.md`](../AGENTS.md) holds the development conventions for this
  repository.
- [README.md](../README.md) carries current status.
