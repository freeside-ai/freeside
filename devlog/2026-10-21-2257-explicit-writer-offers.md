# Every Baseline Writer Names Its Model And Effort

The owner found the last real run's specifier and implementer on Sonnet
5.0: the baseline tree put all three writer roles on the agent
`claude-code-default`, whose offer `claude-code-native-default` names no
model and whose effort is `harness_default`, so the writer launch passed
neither `--model` nor `--effort` and the Claude CLI ran its own default. The
review and judgment roles had already moved to explicit offers (#1616); the
writers were the one role set the baseline still left to the CLI. This note
records the owner's picks and how adoption now writes the writers.

## Owner Picks

The owner chose these pairings on 2026-10-21, replacing the tracker's
2026-09-29 notes:

- **Specifier:** Fable 5.1 at `high`.
- **Implementer and remediator:** Opus 5.5 at `high`. The owner named
  "implementation"; the remediator is the implementation writer of the
  remediation stage, so it follows the implementer (an agent assumption the
  owner can veto).
- **Judgment roles:** Opus 5.5 at `medium`.
- **Reviewer:** `gpt-6.1-sol` at `high`.

The exact Claude ids go in the deployment's settings, not in code: the
daemon writes a model id as given and refuses a completion that reports
another, so the operator gives the id the pinned CLI reports.

## Decisions

Chose to require a model and an effort for every writer role at adoption
over keeping `harness_default` as a writer option. A writer that asks
nothing runs whatever the CLI defaults to, which is the failure the owner
hit, and the baseline is meant to be honest about what each launch passes.
The native-default writer agent and its offer are no longer emitted. A tree
adopted earlier still launches (the daemon does not refuse a ward role on
that offer); the README's upgrade steps move it.

Chose one writer agent per distinct model and effort pair, named
`claude-code-<role>` for the first of `specifier`, `implementer`, and
`remediator` that asks it, over one agent per role. Three agents with equal
bodies would be three names for one digest, and three attended marks for one
launch; the owner's picks give two pairs, and the shared agent keeps the
tree as small as the selection. Rejected naming the shared agent for the
model (`claude-opus-high`), because the tree never trusts a name and a role
name says what the agent is for.

Each writer agent gets an attended mark for the writer launch, although no
run has yet launched under its digest. That is the precedent the review
agents set when they moved to explicit offers: the mark is the operator's
attestation, made by committing the patch, that the agent and launch pair
runs attended under their watch, not a record of a launch that already
ran. The `Tree` doc comment now says so.

Chose a pinned offer per writer pair (`identity_stability: pinned`, the
model as `route_model_id`, one `allowed_efforts` entry), the shape the call
and shadow review offers already use, over a shared offer that allows every
effort. One offer per pair keeps the agent's digest sensitive to the effort
it asks and leaves a hand-authored line one offer to change.

Chose to make `-judgment-effort` required with `-judgment-model` over
leaving the call agent at `harness_default`. The call adapter v2 already
sends every effort; the baseline just had no flag for it, so the owner's
`medium` could not be written without hand-editing the tree.

Chose to carry the writers and the judgment effort through the deputy
settings (`writers.<role>.model`, `writers.<role>.effort`, `judgment.effort`)
with the same required-and-all-or-nothing rules as the flags. The deputy
refuses a tree that differs from adopt's emission, so a setting that could
be omitted would silently re-adopt the writers onto nothing.

Rejected refusing, at daemon startup, a writer line on an offer that names
no model, as the review and judgment roles are refused. The review refusal
exists because a review launch always passes a model; the writer launch
passes one only when its agent names it, and refusing would stop every
deployment on the old tree before the operator re-adopts. That gate can come
later as its own change once every deployment is on explicit writers.

## Revisit When

The Claude CLI exposes an effort the Freeside levels cannot name, a writer
role needs a model the pinned CLI build does not report under the given id,
or a deployment wants per-project writer selection, which belongs in a
project lineup override rather than more adoption flags.
