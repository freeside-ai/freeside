# Routing Policy Record

Work unit #2390, plan §5.4 routing policy, revisions 99 and 103. The record
ships ahead of the router (#2393) and the client surface (#1719), so nothing
reads it yet.

## Chose An Integer Revision Over A Content Address

`TaskLineProvenance.PolicyRevision` already cites a policy as an integer from
1, and the issue contract says no content-address id is needed. The cost is
that a body edit touching only `roles` is invisible: no column repeats the
orders, so the row's cross-check (`scanRoutingPolicy`) sees no disagreement.
Task lines catch that edit through their id. The threat is a hand edit of the
daemon's own database, which also could rewrite `task_lines` columns and
recompute ids, so a roles digest column would add a check nobody needs yet.

Revisit when a client or the router treats a past revision as evidence of what
an operator approved, rather than as a citation.

## Chose To Check The Tree Before The No-Op

`SetRoutingPolicy` resolves every agent before it compares orders, so asking
for what the current revision holds, from a tree that dropped one of those
agents, is refused and not reported as held. A repeat that passes keeps the
current revision's `lineup_revision`: the contract says the same policy records
nothing, and a revision that only moved the tree citation would give the router
a new number for the same rule.

## Chose To Refuse A Relative `-agent-tree`

The plan forwards the CLI args to a running daemon as they are. A relative path
then resolves through `git -C` against the daemon's working directory, so the
same command would read one checkout stopped and another running, or fail only
while running. Refusing it keeps both paths identical. The alternative was
rewriting the path before forwarding, which `canonicalControlArgs` does for
`-db`, but that adds a second appended flag to a list whose trailing `--` it
already has to strip, for a path an operator can write out.

## `set_by` On The Daemon Path

A running daemon runs `set` in its own process, so `user.Current()` is the
daemon's account. The control socket's peer check admits only that account, so
it is the caller's too. No flag carries the name: a caller-supplied identity
would be unauthenticated text in the record.
