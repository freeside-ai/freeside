# Set The Routing Policy Before An Exit Run's Real Submit

Decision record for #3060. The Wave 9 exit run (#2386) submitted its real
task for gh-imgup#134 at 19:45:53Z and set the routing policy at
19:54:46Z. The router reads the current revision at a task's first
admission, so the task ran on the lineup and the routing gate needed a
composite proof built from other tasks. The note is mandatory because
`daemon/cmd/freesided/prelaunch.go` is a Tier A path; the change there
only reads and reports.

## Set After The Deputy's Start, Read Back Before The Submit

Chose to set the policy once the deputy's self-check passes and read it
back with `routing show` before the submit, over requiring it before
`prelaunch`. `routing set` refuses an enrollment the store lacks, and a
fresh root gets its enrollments only when the deputy's start runs `auth
adopt`. Before that start a fresh root can't hold a policy at all, so a
pre-`prelaunch` rule could never pass there. The read-back is the binding
check: a revision whose orders or drain-first account differ from the
exit contract, or none, stops the run before the submit.

Rejected adopting the enrollments in `prelaunch` so the policy could be
set earlier. `prelaunch` runs before the deputy and writes nothing a
deputy start reads; adopting would make it a second writer of the store
and of the agent-tree patch the start emits.

## `prelaunch` Reports The Policy And Doesn't Judge It

Chose to record the current revision in `prelaunch.json` and pass the
`routing_policy` check whether or not one is set, over failing on either
state. `prelaunch` has no exit contract to compare orders with, and most
runs don't route. The report matters on a retained root, where a revision
an earlier run left will route the real task; the exit-run skill compares
it with the contract or records it. The check fails only when the store
read fails, like the other `state` reads, and is `not_run` where the
database isn't read.

## Refute-First Pass

- **The issue-token code changed: disproved.** The `prelaunch.go` hunks
  are the record field, the check and its evidence, and three skip-list
  entries; `mintPrelaunchIssueToken` and `readManagedIssue` are untouched.
- **A passing run now fails: disproved.** The check fails only when the
  store read fails. A routing row that fails its decode is a Tier B bad
  row that fails loudly, and the router would fail the same read at the
  real task's first admission, so failing before the spend is correct.
- **The check writes state: disproved.** It reads through a `ReadTx` on
  the read-only store, under the lock `prelaunch` already holds.
- **The evidence leaks a secret: disproved.** It names enrollment and
  usage-pool ids, which `routing show` and `auth list` already print, into
  the private `<root>/daemon/prelaunch.json`.

Revisit when `routing set` accepts an enrollment the store lacks, or a
fresh root holds its enrollments before the deputy's first start: then
the policy could be set before `prelaunch`, and `prelaunch` could check
it against a contract the operator passes.
