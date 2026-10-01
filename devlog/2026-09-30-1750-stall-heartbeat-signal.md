# Raise a Stall Notice From Provider Response Bytes

Work unit #1631, plan §5.12: "The stall heartbeat (1B.1) is observed by ward
or the daemon and may only accelerate a stall notice. It never resets or
extends any hard budget, and agent output cannot influence it."

## Chose Provider Response Bytes as the Heartbeat

The heartbeat is the time of the last byte a provider sent the writer
through ward's provider proxy. The proxy runs in the daemon and is the
writer's only route out of its container, so ward reads the signal without
asking the agent. Only the provider-to-writer leg stamps it; bytes the
writer uploads don't count. Rejected:

- **Container CPU or I/O stats.** They need a new runtime call and a poll
  loop, and a busy-looping agent could fake progress with them.
- **Agent output (transcript, workspace files, the outcome marker).** The
  plan forbids it: the agent could suppress or fake the notice.
- **A stall field on `exec.Inspection`.** That is a shared contract, and
  `Inspection` carries only liveness. The engine doesn't need the stall; the
  composition root files the notice directly, as it already does for other
  `system_health` items.

Two limits follow, stated in the issue contract for the owner to veto:

- **A process in the container can make a provider send bytes**, for
  example by opening connections. That can only delay the early notice. It
  can't move the hard budget.
- **A long tool command looks like a stall.** The notice is advisory and
  resolves itself when provider bytes flow again.

## Chose Calls Off the Wait Path Over Inline Calls

The watch rides the writer wait's existing polls, so the poll count and
deadline are unchanged. Each hook call runs in its own goroutine, one at a
time. An inline call would let a slow store write delay later polls, and
near the deadline a delayed poll can miss the writer's stop, which changes
the handoff's result. When the wait ends, the watch waits out any call in
flight before it concludes a reported stall, so a late stall report can't
land after the conclusion.

## Chose the System Subject Over the Run Subject

The plan proposed a run subject. Implementation found that an open
run-subject item with any requested decision makes `freesided observe`
report `attention_required`, and `scripts/run-real-work-supervision.sh`
fails a supervised run whose specification lane reaches that state. A
self-resolving advisory notice must not end a run. The notice uses the
system subject and names the invocation in its reason, like the other
daemon-filed health items.

Revisit when: supervision distinguishes advisory health items from
decisions it must wait on, at which point a run subject would let clients
group the notice with its run.
