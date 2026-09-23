# Make Stop Require a Durable Stage Disposition

Issue: #1516

The retained run-113 ward journal closed `canceled` with cancellation intent and
without writer completion or an export. The matching stage intent stayed
`running` and had no execution outcome. The stage pipeline deliberately refuses
to commit a non-completed result from `phaseRunning` based only on a handoff
error. `CancelAndConfirm` checked the fresh ward absence proof but did not
require the stage's durable terminal transition. A controlled live-handoff
regression reproduced that missing result.

A second defect can prevent even the first accepted Stop from confirming:
after its first `StopRun` succeeds, the coordinator deliberately runs
`StopRun` again for a final inventory. The second `CancelAndConfirm` calls
`RequestCancellation` on the now-closed canceled journal. The durable store
rejects the amendment with `ErrHandoffJournalClosed`; the failed second pass
prevents acknowledgement. The production stage recovery launcher can hit the
same path behind the accepted task fence. The connected regression repeats
`CancelAndConfirm` after closure; with the original stage and ward files
overlaid, it fails on that second call with `record is closed`. Ward now
accepts the repeated request only when a fresh read validates that the same
run already has durable cancellation intent. The terminal result
and fresh absence audit are still required for Stop.

Chose to recover ward's closed disposition before Stop confirms. The stage
uses its existing authenticated recovery and outcome commit. It retries
retained terminal writes against the original admission, so later policy drift
cannot make an admitted writer impossible to stop. Fresh owned-resource
absence remains a separate requirement. A closed journal, a canceled provider
return, `live=false`, and successful object deletion each omit part of the
proof, so none grants acknowledgement by itself.

The original `failed_to_stop` acknowledgement did not retain a component
error, and the daemon log recorded only the interrupted writer wait. The
retained verification directory has no owned command records for this run;
the retained judgment record belongs to another task. Current runtime lists
show no object with this handoff's ownership token, but they cannot prove its
state at the Stop request. The source and controlled reproduction identify a
concrete stage-adapter error that can cause `failed_to_stop`; the historical
receipt cannot establish whether it was the only error in that request.
Component-tagged Stop errors now reach the private daemon log for a future
production failure.

Refute-first checks tried to confirm despite a surviving owned container,
an unavailable runtime listing, an unobservable row, and a failed terminal
write. Each kept confirmation unavailable; a fresh retry succeeded after the
recoverable failure. The cancellation journal kept `WriterComplete=false` and
released no export. Reopening the terminal stage intent collected `canceled`
without another writer launch. A connected regression drives the real
Claude-configured stage driver through journaled ward over a controlled
runtime, then reopens both components and collects the same result. Existing
ward checks still reject foreign same-name replacements and incomplete
teardown.

Revisit when a fresh production Stop supplies the failing component and error,
or when ward's recovery or exact-ownership contract changes.
