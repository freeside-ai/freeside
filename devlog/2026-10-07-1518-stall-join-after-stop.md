# Stop the Writer Before Joining Advisory Notices

For #1833, retain the stall watch in the handoff's run state and join it
after deferred teardown and journal closure when the writer wait fails.
When the wait observes a stopped writer, keep the existing join before
container deletion and export, then clear the saved watch to avoid a second
notice clear.

This implements the ordering decision in the
[Wave 8 exit audit](2026-10-07-1159-wave-eight-exit-audit.md#stop-the-writer-before-waiting-for-advisory-storage).
An advisory store write must not keep a credential-bearing writer alive
after its execution budget expires. The final clear still follows any
in-flight raise, and handoff still waits for both calls before returning.

Chose the existing deferred teardown over a second inline stop path because
teardown already checks ownership before reaping resources. Chose a join
after journal closure over one immediately after teardown because advisory
storage should not delay the durable terminal record either. A teardown
failure still leaves its error and recovery record intact; this change
cannot guarantee a successful stop when the runtime refuses it.

The provider proxy gains an unexported listener source so the real handoff
can run under `testing/synctest`. Its test listener blocks on a channel
created inside the bubble; a real socket accept prevents virtual time from
advancing. Nil retains the existing TCP listener and admission rules.

## Refute-First Evidence

- The regression observes the fake runtime's writer stop call. A failed
  notice raise leaves the handoff error unchanged and sends no clear.
  Budget expiry and caller cancellation stop at the same virtual instant
  with and without a slow hook.
- Restoring the old join order in a temporary overlay reproduced the
  defect: a deadline-bound raise delayed the stop from 4 seconds to 2m3s
  on budget expiry and from 3.5 seconds to 2m3s on cancellation.
- A slow successful raise is joined before the clear, and the clear is
  joined before handoff returns. A writer that stops on its own still
  succeeds with exactly one raise and one clear.
- Independent review found no implementation defect. It identified two
  test gaps, now covered: cancellation must be the actual wait error, and
  successful recovery must clear the notice before writer deletion.
- Cancellation journal writes still precede teardown. They and production
  stall notices share the store connection, so this ordering fix does not
  prove independence from shared-store contention. It removes the explicit
  join dependency without changing the durable cancellation protocol.
- A hook that ignores its context may still delay handoff's return. This is
  an explicit non-goal of the issue; it no longer delays the teardown
  attempt. Ownership checks and teardown errors remain authoritative.

## Revisit When

The stall observer gains lifecycle authority, or handoff gains another
background real-I/O operation that prevents virtual-time regression tests.
