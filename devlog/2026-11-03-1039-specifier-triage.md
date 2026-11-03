# Triage Picks the Specifier's Model

Plan revision 106. Owner decisions of 2026-11-03, made in session.

## Owner Picks

The owner wants specification back on Opus 5.5 for most tasks, with Fable 5.1
kept for the hardest: large contract changes, plan or architecture changes,
and complex UI work. A judgment mechanism, not the operator, should decide
which. This replaces the specifier pick in
`devlog/2026-10-21-2257-explicit-writer-offers.md` (Fable 5.1 at `high` for
every task):

- **Specifier, standard tier:** Opus 5.5 at `high`.
- **Specifier, escalated tier:** Fable 5.1 at `high`.
- **Triager:** the judgment roles' agent, Opus 5.5 at `medium` today.

The other writer, review, and judgment picks are unchanged. The exact ids go
in the deployment's settings, as before.

## Decisions

Chose a triage call before specification over self-escalation by the
specifier (owner pick). The specifier has more context when it judges, but a
task it escalates pays for two specifications, and the choice isn't known
until the first one ends. Triage costs one small call per task and fixes the
model before any specification spend. Also rejected: a fixed rule over labels
or paths, which can't see "complex UI work" and would tie the criteria to one
project's labels.

Chose escalated lines on the specifier's lineup line over a second
specifier role. A second role would need its own prompt, and the specifier's
prompt doesn't change with the model; the plan's no-overlay rule keeps one
prompt name meaning one text. The line follows the shadow-line precedent: one
optional extra agent beside the primary, overridden per project like it.
There is one escalated line per agent the specifier can run on, not one per
role, because a routing policy orders several specifier agents, one per
pool, and each needs its own counterpart.

Chose to require each escalated counterpart to share its agent's enrollment
and adapter. That makes the triager's answer a model and effort choice only, which
is what lets a model answer pick an agent at all (below).

Chose to narrow the bounded-choice rule over routing the choice through the
operator. Plan section 5.13 said no answer ever picks the agent that runs
anything; the owner's goal needs a model answer to pick the specifier's
model. The narrowed rule keeps what the old one protected: no answer picks
the vendor, adapter, credential, or usage pool, and cross-vendor driver
selection stays unchoosable (the standing owner decision). An answer may pick
an agent only between two an operator's lineup line offers for the purpose.

Accepted that issue content steers the tier (owner pick). The task-line rule
says intake never sets a line because an agent choice is a credential choice.
Here the two agents share an enrollment, so the credential doesn't move. The
remaining harm is an issue written to look complex spending the escalated
model's allowance on its own specification; the timeline shows the choice and
the operator can move the specifier back.

Chose to fail safe to the escalated tier (owner pick). A failed call shouldn't
quietly give a complex specification the smaller model. The cost is escalated
allowance when the call fails, which should be rare.

Chose to write a line only on escalation. `standard` leaves the specifier on
its lineup agent, so a deployment without an escalated line, or a triager
that never escalates, behaves exactly as today. Either answer still shows on
the timeline through the call record.

Chose to route an escalated task through the counterparts over a separate
escalated order in the routing policy. The router walks the specifier's
existing order, skips an agent with no counterpart, and reads the
counterparts' windows, so the policy needs no new field, can't fall out of
step with the lineup, and the task still gets one pool for every role
(revision 103). An escalated task whose counterparts are all exhausted parks
like any task and never drops to the standard tier on its own; the operator
can move it.

Chose one triage record per task, written for every outcome, over citing the
call. A spent budget or an unbound triager line escalates without a call, so
an escalated line needs something to cite, and admission checks that the
record is the task's own. A consequence the owner should know: a spent
budget or an outage on the triager's pool escalates every task until it
recovers, so the budget caps call spend, not escalations.

## Work Units

The contract unit carries the escalated lines, the triager role name, the
triage record, the triage-sourced line source and its admission anchor, and
the adoption and deputy settings for the escalated specifier agent. It also
lets the operator escalate a task by hand with a task line before the
triager exists. The triage unit adds the site, its prompt, its line write,
and the router's walk through the counterparts, and starts after the
contract unit and the router (#2393).

## Revisit When

The triager's choices disagree often with what the operator would have
picked (task-line changes after triage are the signal), another role needs
tiers, or the shadow line shows a cheaper triager agrees with the primary.
