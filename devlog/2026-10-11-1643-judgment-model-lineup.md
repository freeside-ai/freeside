# Take Every Judgment Model From the Lineup and Remove `-judgment-model`

Work unit #1964, plan §5.4 (Admission, call record). Follows #1619
(`devlog/2026-10-11-0927-judgment-call-model-effort.md`), whose "Revisit
when" line left one question to this unit: what a judgment line on the
native-default offer launches with once the daemon's `-judgment-model` flag
is gone. The issue body carries the contract. This note records the two owner
decisions that answer the question, what follows from them, and where the
implementation left its plan.

## Owner Decisions (2026-10-11)

**The baseline call agent moves to an offer that names a model, and a
judgment line on the native-default offer is refused.** `freesided auth
adopt` writes a second Claude offer, `claude-code-call-default`, and the call
agent names it. Admission leaves a judgment role unbound when its line's
offer names no model: the sites return their fail-safe, the role's
`system_health` item says the offer names no model, and preflight's
`judgment_roles` check fails. Two options were rejected:

- **Launching such a line with no `--model`.** Both hand audits cover a
  launch that always names a model, and the driver refuses a completion from
  any model but the one it passed. A launch with no model is one nobody
  audited, and it leaves that check with nothing to compare.
- **A model constant in the daemon.** The call record would keep saying
  `claude-code-native-default` while a named model was sent, so the record
  and the launch would disagree by design.

**`auth adopt` takes the model, over a model id written into
`agentbaseline`.** A new `-judgment-model` flag on `freesided auth adopt`
supplies the call offer's `route_model_id`, as `-review-model` does for the
review offer. The operator keeps choosing the model. A baseline constant
would need a code change and a release each time that model retired.

## What Follows

**Adoption without `-judgment-model` writes no call agent, no call offer,
and no judgment line.** The only offer adoption could put those lines on
without a model is the native-default one, and the first decision makes such
a line dead: every role would be unbound from the moment the patch was
committed. Writing nothing says the same thing with no dead lines in the
tree, and a deployment that runs with judgments off needs none. For the same
reason `-judgment-publication-author-prompt` without `-judgment-model` is
refused: naming the prompt asks for a line adoption cannot write, and
silently dropping it would turn the publication author off without saying
so.

**Adoption checks the value's shape, not that the model exists.** It refuses
a value `inference.ValidLaunchValue` refuses, and it refuses
`claude-code-native-default`, which would write a second offer that names no
model. An alias or a short name is still written as given. Such a line
admits, and every call on it then fails the driver's exact-model check as
`inference unavailable` with no health item. That is #1619's stated limit,
and checking the id against the CLI at adoption is a non-goal here.

**Upgrading is a manual tree edit.** A tree adopted earlier has every
judgment line on the native-default offer, so on the new build every
judgment role is unbound until the operator changes the tree, and the drift
auditor's item blocks unattended admission meanwhile. A second `auth adopt`
on an adopted root reports each identity `reused` and prints the whole
baseline tree as a patch of new files, which does not apply over the
committed tree. The operator copies the call agent's parts from it: the
offer, the agent, the judgment lines, and their `agents.lock` entries. The
second adoption has to name the terms basis date the committed route
carries, because that flag defaults to today and the call agent's digest
covers its route. `daemon/README.md` (Select Agents Through The Lineup) has
the steps. The
owner accepted this cost with the first decision; rewriting a committed
tree for the operator is a non-goal.

**The judgment configuration digest no longer covers a model.** The model
was in `claudeinference.Config`, which the digest marshals. A digest from an
earlier preflight stops matching, so a deployment reruns preflight after
upgrading. The model is now covered where every other lineup fact is: by the
agent tree commit.

**A retained real-work session that had judgments on cannot resume across
the upgrade,** as after #1426. Its recorded composition names the old
digest, and the harness refuses a resume whose new composition names
another. The resume stops before that comparison: the harness runs the
session's saved binary through preflight with the flags it gives the new
binary, and a binary saved before this unit still requires
`-judgment-model`, so its `judgment_configuration` check fails before the
runtime upgrade starts. Passing the model to the saved binary alone was
rejected. Its preflight could then pass, the harness would mark the
upgrade started, and the resume would still be refused afterwards, at the
agent tree check or at the digest comparison.

## Where Implementation Left The Plan

- **The baseline offer and the adoption flag are one commit.** The plan had
  them as two. A tree whose judgment roles have prompts and no model is an
  error as soon as the baseline changes, so every adoption test was red
  until the flag existed. The harness helper's change went in the same
  commit, because adoption now refuses the author prompt without a model.
- **With no model the call adapter is left out too.** The plan left this
  open. Nothing else names the adapter, and a tree renders and locks with or
  without it, so an adoption with judgments off carries no call fragment at
  all.
- **The refusal lives in `inference` admission only.** A ward launch on the
  native-default offer still passes no model by design, so the rule is not
  one `domain.DeriveAgentLaunchSelection` can hold for both. The driver
  refuses an empty model as well, at the exec boundary, since it no longer
  has a model of its own.

Revisit when #1424 lands a conformance record for the call launch (a
recorded run could check the model id at adoption and close the alias
limit), when a real run adopts an alias and loses its judgments silently,
when #1966 or #1967 move the remaining judgment flags (the upgrade steps
then change again), or when operators need a different model per judgment
role at adoption instead of by patch.
