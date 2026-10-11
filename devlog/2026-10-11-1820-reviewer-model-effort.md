# Take the Review Model and Effort From the Reviewer Agents

Work unit #1965, plan §5.4 (Admission). The reviewer and shadow reviewer
agents now name the model and effort their launches pass, and the four
daemon flags that carried them are gone. The issue body carries the
contract. This note records the owner decision on order, the five planning
defaults the owner let stand, the options they reject, and two findings from
implementation.

## Owner Decision (2026-10-11)

**This unit lands before #898.** #898 records the reviewer's model and effort
from the admitted agent, so the agents have to carry both first. The issue
was filed the other way round, as `starts-after` #898; that order would have
had #898 record values read from flags and then be reworked.

## Planning Defaults the Owner Let Stand

The issue listed five defaults the owner could veto before implementation.
None was vetoed, and the implementation keeps all five.

**No new `EffortLevel` value.** The Codex table maps `low`, `medium`, and
`high` to themselves and `max` to `xhigh`, Codex's highest level;
`harness_default` sends no effort. Rejected: adding `xhigh` or `minimal` as
Freeside levels. That changes the shared vocabulary for values nothing uses
today (the harness and every fixture use `high`). The cost is stated: a
deployment whose flag named Codex's `minimal` or Claude's `xhigh` cannot keep
that value. It picks another effort and approves the new review
configuration digest.

**A review launch needs a named model and an explicit effort.** The daemon
refuses a review role whose agent sits on the native-default offer or asks
`harness_default`, and `auth adopt` refuses to write such an agent. Rejected:
launching a review on the harness's defaults. Every review launch today
passes a model and an effort, so a launch with neither is a new command
shape with new configuration digests and no approval behind them.

**The Codex reviewer gets a second adapter, `codex_review_v2`.** v2 differs
from v1 only in the efforts it can send. Rejected: widening
`codex_review_v1` in place, which would change its digest in every committed
tree and so the digest of every reviewer agent, without the operator editing
anything. v1 stays defined and keeps its conformance record, as
`claude_code_call_v1` did (`devlog/2026-10-11-0927-judgment-call-model-effort.md`).
A test writes v1's digest out so a later edit to the shared builder cannot
move it unnoticed.

**`freesided auth adopt` writes the values into the tree.** It gains
`-review-effort`, `-shadow-review-model`, and `-shadow-review-effort` beside
`-review-model`. An effort is a Freeside level, not a harness string. The
shadow reviewer gets its own agent, `claude-shadow-review-default`, on its
own offer under the Claude route. Rejected: moving the writer agent
`claude-code-default` to an offer that names a model. The writer roles would
then launch with `--model` and `--effort`, which they do not today, and
every writer admission would be a different agent. The writer agent's digest
is unchanged, and a test writes it out.

**The harness keeps its two variables.** `FREESIDE_REAL_RUN_REVIEW_MODEL` and
`FREESIDE_REAL_RUN_REVIEW_REASONING` now feed `auth adopt` only, and the
second is a Freeside effort level. This replaced the filed acceptance line
that had the harness stop reading them: the harness adopts a tree on every
run, and that tree needs both values.

## Findings From Implementation

**The pinned Codex build does not check the effort.** `codex-cli 0.147.0`
was run with `model_reasoning_effort` set to `xhigh` and to a string that
names no level; it accepted both and would pass either to the service. So
the build does not reject `xhigh`, and the issue's fallback (leave `max` out
of the table) did not apply. Whether the service accepts `xhigh` for a given
model was not verified: the probe ran without network. A model that refuses
it fails at its first review, not at adoption or admission.

**Changing the launch derivation moves no stored binding.**
`DeriveAgentLaunchSelection` now returns the offer's model and the
translated effort for a `codex_cli` agent, and the reconstruction recheck
(`ValidateAdmissionAgentDerivations`) trusts that derivation, so a stored
binding for a Codex agent written in the old empty form is now refused. No
such binding exists:

- The engine's admission resolver is the only writer of a binding, and for
  every ward launch it refuses a review adapter at capability coverage,
  before it reaches the effort. A writer launch needs mutation tools the
  review adapter lacks, and the review launch needs an auxiliary-inference
  control no adapter has proved.
- Wardless admission refuses any adapter that is not `claude_code`.
- An operator-authored Codex adapter has no conformance record, so it is
  refused at the same gate.

The same gate is why v2's wider effort list does not make a Codex agent a
writer; the conformance test covers both review builds. An old and new
derivation were compared over 360 generated inputs: they agree on every
Claude input and differ on exactly the 45 Codex inputs that pin a model,
which is the intended change.

**Adoption applies both launch-shape rules to a review model.** The review
launch's own rule (`ward.ValidateCodexReviewModelConfiguration`) refuses a
comma and control characters; `inference.ValidLaunchValue`, which the
judgment model already gets, refuses whitespace and a leading `-`. Adoption
refuses a value that fails either, for `-review-model` and
`-shadow-review-model`. It does not check that the model exists.

## What an Upgrade Costs

A tree adopted before this unit cannot run a review on this build: its
reviewer asks `harness_default`, and its shadow reviewer's agent names no
model. The daemon holds admission and preflight fails, each naming the role,
until the operator commits the review agents from a fresh
`freesided auth adopt`. The new tree is a new lineup revision, so preflight
runs again and a retained real-work session adopted on the old tree cannot
resume. The approved review configuration digests do not move when the
agents name the values the flags named. The owner accepted this with the
second default; rewriting a committed tree for the operator is a non-goal,
as it was in #1964.

Revisit when #898 lands review admission: a review then records the
requested effort beside the native one, so the `max → xhigh` clamp becomes
visible on the run. Until then a `max` reviewer shows as `xhigh` with no
clamp recorded. Revisit the Codex table when the review image's Codex pin
moves to a build that validates the effort.
