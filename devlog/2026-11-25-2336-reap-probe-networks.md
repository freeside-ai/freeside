# Rig Recovery Reaps Probe Networks, and Dispatch Waits for Discovery

A killed lineup probe or model discovery left a network no journal owns, so
`rig recover` refused over it and every later start on any root waited on a
person (#3447, exit-run findings F6 to F8 from #2382). Recovery now deletes a
recorded lineup probe network once its labels bind it to its recorded name;
a refusal over leftovers names the command that clears them; and the daemon's
startup model discovery finishes before the engine dispatches a task's first
launch. Tier A by path (`cmd/freesided/rig.go`, `internal/ward/`,
`internal/wardname/`) and by category (destructive cleanup).

## Chose the Manifest Name Plus Labels Over a Probe Journal

The probe already passes its container and network names to the rig
authorizer before it creates either, so the manifest records them
write-ahead. Recovery trusts that name, then re-checks from one fresh listing
that the network shows exactly one run label `lineup-probe-<slice>` and
exactly one 32-hex ownership label starting with `<slice>`, the slice the
name carries. Any present probe network that fails refuses the whole reap
before anything is deleted. Deletion runs through the ward's ordinary
`teardownNetwork` under the observed ownership label, which re-lists and
proves the network gone.

A probe journal recording the full ownership token and creation instant
would add a fingerprint, but it is a new durable record and write path for a
probe that lives for minutes, and the fingerprint guards against a
same-name replacement, which a 48-bit random slice inside this rig's own
recorded namespace already makes negligible. The labels come from the
container runtime, which the threat model treats as trusted host state.

No label sweep runs without a rig lease: two instances on one host share one
runtime, so a sweep by label alone could delete another instance's live
probe. Recovery acts only on names this rig recorded.

## Chose Dispatch Waiting for Discovery Over Discovery Waiting for Launches

Startup discovery ran in a goroutine beside the engine loop, and its memory
reservation only ordered it ahead of a launch when memory was short. The
engine now takes a dispatch hold (`engine.WithDispatchHold`) that the
discovery pass closes on every return, and as soon as it first waits on an
update drain or an execution pause, since those hold launches too. The rest
of each engine pass runs as before.

Making discovery wait for launches instead would leave its catalogs stale
for as long as tasks keep arriving, and a stale catalog blocks selections.
The cost of this order is a startup delay of at most one discovery pass,
about one probe per enrollment, each bounded by the probe and export
timeouts. Reviews, verification, image upkeep, and enrollment-command
discovery are not gated.

## Refusal Text Names a Command

`rig cleanup` and the deputy's release name
`freesided rig recover -state-root <root> -confirm` (the deputy adds that its
next start on that root recovers them). `rig recover` itself has already run
every recovery it has, so what remains has no owner it knows; it names the
`container delete`, `container volume delete`, or `container network delete`
command for each remaining name, for a person to run after checking nothing
uses it.

A refused reap no longer stops recovery: the review journal still recovers
its own networks, and the final absence proof names the hand delete for the
probe network the reap left, beside the refusal itself.

## Refute-First Review

A fresh-context reviewer, prompted to refute, found no blocking defect. It
confirmed that the reap deletes only a recorded probe name whose labels bind
it, that the dispatch hold closes on every discovery return, that
`dispatchPendingInvocations` is the only path to `driver.Start`, and that
the hold cannot deadlock against the probe's memory wait, which already
ranks the probe ahead of launches.

- **Fixed:** the hand-delete advice said no journal owns what is left,
  which overclaims after a refused reap; it now says recovery could not
  clear them and asks for a check first.
- **Fixed:** a refused reap returned before the review journal ran and
  named no command; it now runs the journal and joins the refusal with the
  absence proof's advice.
- **Declined, a deadline on the hold:** every bound a deadline would add
  already exists as the probe and export timeouts, and a launch released
  early would contend for the same `container` CLI the probe is waiting on.
- **Declined, held passes skipping the run-hold observation:** the
  observation is display-only and resumes on the first pass after the hold
  closes.

Revisit when a lineup probe gains its own journal (#2655 may add one): the
reap should then defer to it rather than to the manifest name and labels, or
when an update drain is canceled without a restart often enough that
discovery and dispatch resuming together matters.
