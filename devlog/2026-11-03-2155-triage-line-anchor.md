# Triage Line Anchor

Decision record for #2510, the contract half of plan revision 106's
Specifier Triage (plan §5.13, §5.4). The agent decided these points while
executing the issue's implementation plan.

## Admission Re-Checks a Triage Line Against Current State

Chose to anchor a triage-sourced task line at admission against the task's
triage record and the current lineup, over trusting the stored line, because
a decoded task line chooses which agent, and so which credential, runs the
specifier. That makes the anchor a Tier A gate even though the line and the
record are Tier B state.

Each check closes one way a row could put the specifier on an agent no
current policy names:

- **Role is the specifier.** Escalated lines exist only for the specifier,
  so a triage line on any other role has no counterpart to match.
- **SetBy is the line's own task.** The triage record is keyed by task; a
  line naming another task would borrow that task's escalation.
- **The record exists and escalated.** A `standard` answer writes nothing,
  so a triage line beside one has no cause.
- **The agent is the counterpart the current lineup names.** The counterpart
  is read from the specifier's current lineup line and its escalated line,
  at the digest the tree resolves now, never from the task line. An operator
  who removes or repoints the escalated line therefore stops old triage
  lines from admitting the old agent, which a check against the line's own
  stored selection would miss.

An operator task line on the specifier stays the hand escalation path and
needs none of this: an authenticated operator chose it.

## Escalated Lines Carry the Full Selection

Chose the key `lineup.role.specifier.escalated.<agent>` with the full
selection value `<counterpart>@<digest>/<prompt>@<digest>`, whose prompt
must equal the specifier line's, over an agent-only value that would
inherit the prompt. The full form reuses the selection grammar and its
parser unchanged, pins the counterpart's digest like every other line, and
keeps each lineup row readable on its own. Requiring the prompt to match
keeps one specification prompt per specifier whichever tier runs; the cost
is that a prompt change edits two rows, which the resolver refuses to leave
inconsistent.

The pair must share an enrollment and an adapter, checked where the agent
tree resolves its lineup, so escalation changes the model and effort and
never the identity or harness.

## The Triage Record Is Tier B

Chose plain validation on write and decode for the triage record, over
Tier A treatment, because the record grants nothing by itself: it feeds the
anchor, which re-reads it on every admission. A bad row is a bug that fails
loudly, and the record carries no column-versus-body cross-check or tamper
test.

Revisit when the triage record gains a reader that acts on it without going
through the admission anchor.

## Refute-First Findings

A fresh-context reviewer tried to break the anchor before push.

- **Confirmed, fixed:** the counterpart-digest refusal had no test. A
  mutation that disabled it passed the suite; a stale-digest case in the
  anchor's refusal tests now fails under that mutation.
- **Confirmed, fixed:** the migration header said its CHECKs repeat all the
  domain rules; the reason's length, encoding, and blankness are domain-only.
- **Allowed by decision:** the anchor does not bind the line's set time to
  the record's, unlike the submit, proposal, and change arms. Plan §5.13
  names the four checks above; nothing writes a triage line yet, and any
  line that passes still runs only the current counterpart on the
  specifier's enrollment. The triager unit that writes the line decides
  whether to bind them.
- **Allowed by decision:** a triage line does not re-check the primary
  specifier line's pinned digest, as operator lines do not; the escalated
  line's own pinned digest guards the agent that runs.
- **Disproved by check:** an error path that falls back to the lineup agent,
  a counterpart read from the row, another agent's escalated key admitting,
  a pair with differing routes (equal enrollments force equal routes), a
  dotted or empty escalated agent name, a triage line on a review role, and
  a decoded `escalated` bit trusted without re-deriving it.
