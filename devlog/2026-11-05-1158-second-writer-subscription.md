# Second Writer Subscription

Issue #2653. Tier A: the change decides which credential volume each writer
agent mounts and which recorded identities a deputy start accepts.

Chose a second set of writer agents on a second `claude_code` enrollment,
reached through the routing policy, over a second set of lineup lines,
because the lineup selects one agent per role and the routing policy already
orders agents across identities (`routing set -order`). The second
subscription's work is the operator's choice per deployment, so it belongs
in the revisioned routing policy, not in a second lineup that would need its
own role vocabulary.

## Decisions

- **Agents, not lineup lines.** `auth adopt -second-auth-identity` adds a
  copy of each writer agent on the second enrollment. No role line names a
  copy, so a deployment that sets no routing policy runs exactly as it did
  with one writer. The copies are dormant until `routing set` lists them.
- **The `-second` suffix.** Each copy is the first identity's agent name
  plus `-second` (`agentbaseline.SecondWriterAgentSuffix`), on an offer of
  the same name. A fixed suffix keeps the two identities' agents paired by
  name in `agents.lock`, routing orders, and run records. It also limits the
  shape to two writers, which is the issue's scope. A third writer would
  need a naming scheme and is a new decision.
- **One added lineup line: the second escalated specifier.** Triage needs
  `lineup.role.specifier.escalated.<specifier-agent>` on the same
  enrollment and adapter as the specifier (`CheckEscalatedPairs`). Without a
  line keyed on `claude-code-specifier-second`, a specifier routed to the
  second identity would never escalate. The line only takes effect for a
  specifier that the routing policy already sent to the second identity, so
  it adds no new way to reach that credential.
- **The stray-identity refusal covers both writers.** Deputy start refuses
  a root whose store holds an enabled Claude identity with a recorded volume
  that the settings name as neither writer nor the reviewer. A writer the
  settings changed or dropped keeps its enrollment. No agent runs on it,
  because `agentTreeMatches` requires the committed tree to equal
  adoption's patch, which carries only the named identities' agents. But the
  scheduled usage collection still calls the provider under its credential
  (`DefaultPoolCollector` picks a collector for every enrolled pool). The
  fresh-context review first read the refusal as defense in depth; the
  collector is the live use it stops. The check runs on the first identity
  as well as the second. Before this, a changed first identity left the old
  one enrolled without complaint. Fresh roots are unaffected. A reused root after such a change
  now refuses at start, naming the identity, and the remedy is to name it
  again or start fresh.

## Departures From the Plan

- **A second route is refused, not added.** The plan said to add the second
  enrollment's route if it differed. The tree builder refuses it instead.
  The deputy and `auth adopt` both use the default route for both writers,
  and each copy's offer sits on that route. A second route would need its
  own route fragment and terms basis, which nothing asks for.
- **Adopt also refuses a shared volume.** A second identity whose recorded
  volume any other enabled identity records, the first writer or a Claude
  reviewer, is refused before anything is recorded. Two identities on one
  volume would make two usage pools out of one subscription, and the second
  writer's agents would mount the other identity's credential.
- **Second-writer resolution failures surface in
  `second_claude_credentials`.** They do not appear in `agent_selection`. The
  first writer's checks and its one-volume rule for lineup writers stay
  as they were, so the existing preflight goldens are unchanged.
- **`-retire-unadoptable` was not extended** to the second identity. It
  reads the first two report entries by position, and the second writer is
  listed third so those positions keep their meaning. An unadoptable second
  writer emits no patch, so nothing runs on it. Follow-up: #2657.

## Refute-First Findings

A fresh-context reviewer tried to prove the diff wrong before commit.

- **Confirmed and fixed: a second writer could share a Claude reviewer's
  volume.** Adopt compared the second volume only with the first writer's.
  It now refuses a second writer whose volume any other enabled identity
  records. The same gap between the first writer and a Claude reviewer
  predates this unit. Follow-up: #2656.
- **Confirmed and fixed: the production second-writer preflight path was
  untested.** Tests now run tree resolution and the database inspection
  against an adopted two-writer tree, and the deputy's mapping into the
  preflight config.
- **Allowed: an unadoptable second writer cannot be retired in place.**
  Dropping it from the settings then hits the stray refusal, so the remedy
  is a fresh root. That fails closed. Follow-up: #2657.
- **Corrected: the stray refusal's reason.** See the refusal bullet above.
- **Disproved by reading: crossed volumes between the two identities.**
  Each `-second` agent carries the second enrollment's ID, the tree builder
  refuses the first identity's enrollment as the second, and admission joins
  enrollment, identity, and generation, so no agent resolves across
  identities. The baseline test checks the second agents resolve only under
  the second identity's records.
- **Owner's call: tier classification.** The issue keeps `deputy.go` and
  `agentbaseline/tree.go` off the Tier A list, because a human commits the
  tree and admission re-gates every agent. The reviewer disputes it for
  `deputy.go`, which records the volume each writer identity mounts, so
  swapped settings values would mount each subscription's agents on the
  other's volume with nothing downstream to catch it. This unit leaves the
  list as the issue says and raises the question in the PR.

Revisit when a deployment needs more than two writer subscriptions, or when
#2599 replaces agent documents with selection tuples. Either one removes the
reason for a fixed name suffix.
