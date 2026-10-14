# Compose The Deputy Once Per Launch

`freesided deputy` (#1938, Unit 1) runs the real-work rig in one process. It
moves a trust boundary: the production composition manifest used to bind one
submission, and now a launch manifest binds every submission made while the
deputy serves. These are the decisions that shape that boundary, and where
the implementation departs from the plan comment.

**One launch manifest, bound again at each submit.** Chose a launch-scoped
manifest (`freeside-production-composition-v2`, no run identity, carrying
the settings digest, the project, and the publication author) over
composing per submission inside the deputy. The deputy composes before any work is known,
so it has no run identity to bind. To keep the manifest from authorizing more
than it checked, `submit --require-composition` re-binds each submission to
it: the submission's project must be the launch's one project, its commit
author must equal the composed publication author, and its resolved path
allowlist must equal the composed allowed paths. A manifest that is not
passing, or that carries neither a run identity nor a launch binding, is
refused. Per-submission manifests from `freesided preflight` keep their old
binding, and a v1 one still replays, since v1 is v2 without the launch
fields.

**A launch manifest binds only through its running deputy.** The manifest
attests one running composition, so submit refuses it when no daemon serves
the database, and the deputy removes it when it ends and again at every
start. The deputy also re-binds each new submission on its own side of the
control socket, against the digest of the passing manifest it composed: the
socket accepts any prepared request, so a check only in the submit client
would let `submit` without `--require-composition`, or any other client, add
unbound work, and a deputy whose check failed accepts no new work at all.
Found by Codex review. Rejected binding it to the rig lease's token instead:
an abandoned lease keeps its manifest and token, so a crashed deputy's
manifest would still match. Found by the pre-push refute review, as were the
project binding above and the release proof below. The paired client's
`submit_task` is the other way new work arrives; it names no manifest, so
the deputy binds it implicitly, creating a task only for a configured
project whose commit author and policy pass the same check (also found by
Codex review). Re-runs of an existing task (reattempt, capability retry,
revised specification) are not gated at intake: they inherit their parent's
binding, and the Claude driver re-checks each new attempt at admission
against the deputy's current allowed paths and the App bot it resolves for
its configured repository, so a re-run cannot publish outside the launch.
Label intake and scheduler occurrences create no task in a deputy.

**Self-check before the daemon serves, not after the store opens.** The plan
put adoption and preflight after the store opened, so a failure would be a
card on a running daemon. Code reality differs: `composePreflight` takes
`prod`'s database lock itself and probes the listener, so it cannot run
while the daemon holds both. Chose to run it first and then serve without
the Claude driver on failure, filing one blocking `system_health` card
(`deputy_preflight_failed`) that names the failed checks. That keeps the
plan's outcome (a failure is a card, not a dead process) under the real
lock order. A later passing start resolves the card.

**The deputy records the auth identities.** The plan said the verifier
writes nothing. The deputy records the writer's and reviewer's auth identity
from the settings once, before adoption, and refuses a later settings file
that names different ones, because adoption and the driver both read those
records and an unrecorded identity fails them.

**Release cleanly only after proving the rig clear.** `rig hold` released
cleanly only after the script's `rig cleanup` deleted the recorded
containers and proved every recorded resource absent; a plain signal
abandoned the lease. The deputy keeps that rule in one process: at the end
it reads the lease's manifest from disk (the driver and the preflight record
resources there as they bind them), deletes the containers, and releases
only on a passing proof. Anything else abandons the lease for the next
start to recover. Releasing on any signal would delete the manifest that
names leaked resources, leaving no record to recover from.

**Recover only this root's stale lease.** A live holder refuses the launch.
A stale lease whose state root is this deputy's is recovered without the
`rig recover` prompt and leaves an advisory `deputy_run_interrupted` card. A
stale lease for another root refuses and names `freesided rig recover`:
recovering it would tear down another root's review containers on a guess.

**Refuse a duplicate task per project, not store-wide.** The issue asks
`submit` to refuse a second unfinished task for the same source. Chose to
scope that to the submitting project, because the same specification
submitted to another project is new work, which the existing
submission-identity rules already treat as distinct. A deputy admits only
its launch project, so in a deputy's store the two scopes are the same. The
source is the submitted specification artifact; rejected matching on the
publication's source issue, which would refuse a deliberately revised
specification for the same issue. The refusal names the first task and how
to settle it.

**Settings hold what the daemon reads, and nothing script-only.** The
supervision deadline stays `submit`'s `-supervision-timeout`, because it
belongs to one submission, and the script's diagnostic directory has no
daemon reader, so neither is a settings key. This differs from the
acceptance bullet's field list. The self-watching the issue asks for reuses
existing mechanisms: the writer stop and stall timeouts from settings, the
supervision deadline, a failed specification's attention item, and the
durable stop on component exit.

**`prod` still stops by hand.** The deputy holds `prod`'s database lock as
the script did, so the operator stops and restores `prod` around a launch.
Unit 2 changes that; this unit's first proving run cannot show "`prod` keeps
running".

Revisit when Unit 2 lets `prod` keep running (the self-check then can no
longer take `prod`'s lock), or when a deputy serves submissions for more than
one repository, which a single launch manifest cannot bind.
