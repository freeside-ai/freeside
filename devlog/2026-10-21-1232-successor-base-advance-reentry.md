# A Successor Cycle Re-Enters When Its Base Advances

Issue #2111. A feedback return or remediation continuation builds a head
for a pull request the run already published. When the publisher refused
that head because the target base had advanced, the cycle blocked on
`base_advanced`, and its Rerun reviewed the head at the superseded base
again and blocked the same way. The Wave 8 exit run (#1613) stopped on that
card. The daemon now seals a successor re-entry instead.

## A Sixth Successor Version, Not A Wider v5

Chose a new authority, `freeside.publication-successor/v6` with origin
`successor_base_advance`, over letting the v5 unpublished re-entry (#2058)
cover published runs. v5's whole proof rests on the run never having
published: its chain runs through unpublished re-entries only, and its
publication is the run's first. A successor re-entry updates an open pull
request, so it must name that request through the published ancestor's
binding, and its predecessor is the refused cycle's ready item, not the
root. Folding both into v5 would make every v5 read distinguish the two
cases by inspecting the chain, which is the ambiguity versions exist to
prevent.

v6 keeps v5's shape and rules: the base is the daemon's latest audit tip,
the head is kept, only the head's prospective merge into that base stands in
for it, and no remediation is admitted. Its producer is the predecessor
cycle's producer (the feedback invocation or the remediation), and its
publication updates the existing pull request through the successor update
path, whose target head is the published ancestor's.

Rejected: blocking with a clearer reason. The run would still need a fresh
task to land a reviewed, verified head, which is the dead end #2111 names.

## The Replaced Cycle Keeps Its Lineage Segment

A disposition history links review rounds across bases only through the
segments the re-entry chain names. A successor re-entry can follow an
unpublished re-entry while the feedback cycle between them worked at the
admitted base, so that cycle's rounds now open a segment of their own at its
first round. Without it, their dispositions dropped out of the published
history.

## Refute-First Findings

A fresh-context reviewer tried to break the change. No trust-boundary bypass
was found.

- Confirmed and fixed: the lineage gap above.
- Declined as hardening: the v6 producer check by invocation prefix. The
  store gate binds the producer's export to the kept head, and a successor
  update pushes only that head.

## Evidence Limits

The integration tests cover the seal and the v6 publication through the
refusal path, and a second refusal after a further base advance sealing a
second v6.

They drive the feedback origin only. A remediation
continuation refused for a base advance, and store-level negative cases for
the v6 gate, need fixtures that do not exist yet. Follow-up: #2120.

Revisit when: a successor cycle needs remediation after a base advance, which
v6 does not admit.
