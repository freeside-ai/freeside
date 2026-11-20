# Invocation Transcript Read

**Decision (#3506):** a paired device reads an ended writer invocation's
transcript through `GET /runs/{run_id}/invocations/{invocation_id}/transcript`.
The read returns the transcript claim's digest and the prompt the writer
received; the client fetches the bytes from `GET /attachments/{digest}`.
This is Tier A work: it serves sensitive agent output and admission-bound
text to a client, so this note records the boundary choices.

## A Digest, Not Bytes

Chose to return the claim, with its digest and recomputed availability,
over inlining the transcript bytes the way the review-evidence read
inlines a reviewer's output. A failed writer's transcript runs to
megabytes, and the attachment path already carries the device's size cap,
its availability states, and its retained-bytes budget. The card's
transcript route uses the same path, so both routes open the same bytes
under the same gates.

## The Run Binding Comes From the Admission Record

The invocation's recorded execution admission binds it to its run: an
admission naming another run reads as not found, so a device can't reach
another run's transcript by pairing a run ID it can see with a foreign
invocation ID. The read authenticates the stored admission without
applying current policy, because a policy change must not hide history
the operator already has. A reviewer's invocation has no execution
admission, so its output stays on the round's own evidence read.

A claim set that disagrees with the invocation (a transcript naming
another producer, one that is not sensitive and head-independent, one with
inline text, or two transcripts) fails the read with a 500 and no partial
content. The checks are the ones the failure card applies to the same
claim.

## The Prompt Comes From the Admission-Authenticated Intent

The stage driver's `InvocationPrompt` reads the prompt back from the
invocation's stored intent through `loadIntentAdmission`, the gate that
re-renders the prompt from hash-checked inputs and refuses an intent whose
prompt disagrees with its admission. Two options were declined:

- **Re-rendering the prompt at read time.** The render reads the
  protected-path list and other policy that can change after admission, so
  a re-render can show a prompt the writer never received.
- **A new stored prompt record.** It would cover only invocations admitted
  after it shipped, and it would be a second copy of text the intent
  already holds under an authenticated gate.

The read is lock-free and writes nothing: the gate path reads the intent
file, the store, and the ward's export journal, and an intent is replaced
atomically, so a concurrent writer never shows a torn file.

The prompt is daemon-rendered text, not agent output. It is served
unscanned, and only to a paired device behind the bearer check, the same
audience that already sees the work item it is rendered from.

## A Prompt Error Serves a Null Prompt

Departed from the plan's rule that a gate refusal fails the read. A
reader error (a refused, undecodable, or unreadable intent) serves a null
prompt and logs a warning; only the request's own cancellation fails the
read. The gate's refusals include plain, untyped errors, and signet does
not import the stage package, so it can't tell a refusal from an I/O
error. Under the plan's rule, one corrupt intent would have failed the
transcript read for good. The prompt is display-only, and the transcript
is still served under its own claim checks.

## Unavailable Is One State for Now

`availability` is `available` only when a transcript claim exists and the
blob store holds its bytes; no claim and gone bytes are both
`unavailable`. #1926 adds a state for a running invocation's growing
transcript, which is why the enum is not a boolean.
