# Carry Enrollments on the Routing Policy Wire

Work unit: #2807, which ends the interim wire #2736 left in signet
(`devlog/2026-11-08-1247-routing-policy-over-enrollments.md`). The
client contract now orders enrollments in `set_routing_policy`, the
synced policy, and a task line's passed-over mark. Mandatory note: a
`kind:contract` change.

## Decision: Two Encodings on the Wire, No Translation

`RoutingPolicyRole` renders exactly one of `enrollments` and `agents`,
as the daemon stores the revision: every revision since #2736 carries
`enrollments`, and an older one carries the agent names it recorded.
`TaskLinePassedOverMark` does the same with `enrollment` and `agent`.
Chose that over translating an agents-form revision through the
catalog, both inbound and outbound, because a translation depends on
the tree at read time. The interim wire showed the cost: a tree edit
changed what a stored revision read as, and a replay after the tree
dropped an agent became a refusal. With two encodings the wire shows
what was stored, and the shape follows `TaskLine`'s `agent` and
`selection`.

The command takes only enrollments. `RoutingPolicyRoleInput` has no
`agents` key, so the strict decoder refuses a name with 400. The
setter's existing checks (unknown, disabled, or unserved enrollment;
repeated entry; empty order) refuse the rest, and their messages name
the role and the entry's position, never the id.

The cost: an agents-form revision lives on until #2599 converts or
refuses it, and every reader handles both forms. In the app, the pane
shows the stored names, and a change seeds its draft by mapping each
name to its catalog agent's enrollment, dropping a name the catalog
lacks. So the operator's first save from an agents-form revision sends
enrollments for every role, and a role whose names all left the tree
loses its order in that save. The routing doesn't change, because the
router already skipped a name the tree lacks. The editor shows the
converted orders before Save, but a popup choice saves at once, so it
converts the other roles unseen. That was accepted rather than
withholding popup choices until the editor runs, because the daemon
checks every entry and refuses the whole change if a converted
enrollment can't run its role. This is a client-side seed, not a daemon
translation.

## Decision: The Passed-Over Mark Joins This Unit

The issue named only the policy. The mark joined because #2736's
outbound render put an enrollment id in the mark's `agent` field. That
was a name field carrying an id, and it would have stayed that way had
the policy moved alone. The stored mark already records one or the
other, so the wire now renders the field that is set.

## Decision: Drain First Needs an Ordered Enrollment

The app offers a drain-first pool only when an enrollment the draft
orders sits on it (or, with no draft open, one the policy orders).
Before, it offered any pool a catalog agent was on. That mirrors the
daemon's rule, which already refused any other pool, so the app now
withholds a choice the daemon would refuse. The mock applies the same
rule.

## Dropped: The Escalated-Line View

The issue also named "the catalog's escalated-line view". The agent
catalog never carried escalated lines on the wire, and the daemon keys
them by enrollment internally, so there was nothing to rename. Adding
escalated lines to the catalog would be its own unit.

## Classification: Tier B

No Tier A path changes. The routing policy's wire, the operator
command's decode, and the mock are Tier B: the policy feeds routing,
and admission re-checks every routed line against the stored revision
(Tier A, unchanged). The setter's refusals already reach the 400
mapping in `daemon/internal/signet/http.go`, which stays untouched.

## Cache

The app's cache format goes to 12 (#2808 took 11). A format 11
snapshot holds the interim render, and its cursor would skip the
bootstrap that replaces it. An older unsettled routing change ordered
names the daemon now refuses, so that section fails its decode and
loads absent, while the command ledger survives as it does for every
older format.

Revisit when #2599 removes the agents form: the `agents` and `agent`
fields, the draft's seed, and the view's name fallback go with it.
