# Authenticate Feedback Returned After a Remediation by Review Order

Chose to accept a feedback-produced publication task under an older active
remediation when the feedback's sealed successor round shows it came later:
`Successor.ReviewRound > request.Round + 1`. Before this, the publication
lane's producer check recognized a feedback producer only when the
remediation itself was asked to fix that feedback's head. Feedback returned
on a head a remediation already produced therefore read as a foreign
producer, and the run was quarantined. The Wave 8 exit run hit this after
Return to Agent on a remediated head (#2101).

## Why the Round Is Enough

A round-N remediation answers review N. It commits only while review N is
the latest review record, and the review gate stays pending until it
exports, so review N+1 can only cover the remediation's head or a
descendant of it. A successor is sealed with `ReviewRound` one past the
latest review, and only while the ready item covering its head is current.
So a sealed round above N+1 means the feedback was sealed after review N+1
and is the newer producer. The strict `>` keeps an in-place re-entry that
reviews the same head at round N, beside feedback sealed at N+1, on the old
fail-closed path.

The round is not trusted as decoded. `authenticateTaskSuccessor` compares
the task's successor with the stored record, whose read re-runs the
successor validation against the prior review record. The base and
publication comparisons still run first. The skipped head comparison does
not free the head: `loadBinding` still requires the feedback invocation's
own export head to equal the task and replay heads.

## Rejected Options

- **Retire the remediation marker once its head publishes.** That would
  change the marker lifecycle every lane reads, a contract change, to fix
  one lane's producer check.
- **Prove ancestry from the remediation head with git.** That puts
  repository I/O inside a store transaction and makes a git-derived result
  a trust bit, where the sealed review order already answers the question.

## Refute-First Findings

- **Disproved, the ordering invariant has a gap:** traced remediation
  commit and export gating, successor sealing, publication continuations,
  external cycles (re-entered tasks return before this check), and drift
  reversals.
- **Disproved, a tampered round slips through:** the stored-record
  comparison and read-time re-gate catch it. Residual, tampering only: the
  read-time gate does not re-check that the prior review was the latest at
  seal. A rewritten row could inflate the round, but the export-head
  binding above still limits what can publish.
- **Not covered by a test:** two remediations around one feedback, a second
  feedback on a feedback-produced head, and a negative case pinning the
  strict `>`.

Revisit when the review gate stops serializing remediation and review, or
when successor sealing stops requiring the prior review to be the latest.
