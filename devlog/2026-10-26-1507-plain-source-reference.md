# Write the Source Reference and Review Replies in the Target's Terms

**Decision (owner, 2026-10-26, #2349): a published PR's source reference is
the bare `Closes #N` (or `Refs #N`, or the descriptive link) for both closure
provenances, and a declined or deferred external-review reply names who
decided, with no internal digest or placeholder reason.**

The owner objected to two contributor-facing strings on
freeasinbird/gh-imgup#145, a Wave 8 exit-run PR (#1613):

- The PR opened with "Closes #132. The client recommended this issue; the
  approver confirmed it." The task came from the CLI, so "the client" was
  wrong, and "the approver" named no one.
- The decline reply on the owner's own thread read "Freeside declined this
  finding." with the reason "Human decision recorded. (finding adjudication
  sha256:3f7b…)". The person gave no reason, so the reply printed the
  human route's fixed rationale and the engine's adjudication digest.

Both strings are written for Freeside's provenance model, and the PR's
readers are the target repository's maintainers. A provenance mark belongs in
Freeside's records.

## What Changed

- **Plan §1B.1 drops the PR-body mark** (plan revision 100). The
  recommendation stays marked on the proposal, and the `effect_proposal`
  card shows it when the human gate keeps one.
- **The reply words a person's decision as "A maintainer declined this
  finding."** (or deferred), with no reason line. A model or engine decision
  keeps "Freeside declined this finding." and prints the adjudication
  entry's rationale alone. The disposition's stored `Reason` is unchanged.

## Rejected Options

- **Keep the sentence.** It's accurate only in Freeside's vocabulary and
  wrong on a CLI run.
- **Reword it in the target's terms,** such as "Linked by the submitter."
  The maintainer reading the PR has no use for the distinction: either
  provenance was approved before the line was written, and a recommended
  close under the human gate was approved by a person who saw the mark.
- **Ask the person for a reason on Decline or Defer.** That changes the
  `adjudicate_findings` payload, the API, and the app, so it's a contract
  unit (#2352), not this one.
- **Print the reason with the digest stripped from the string.** Parsing
  the engine's `<rationale> (finding adjudication <digest>)` format couples
  the reply to that format. The reply reads the entry's rationale and
  producer instead, which the disposition's adjudication digest names.

## Known Gap

On the default policy, no operator read shows a policy-approved close's
provenance once the PR sentence goes: `closure_cards` print no proposal
parameters, and a policy-approved close has no card. The proposal record
still holds it. Showing it in `freesided inspect` is follow-up #2359.

"A maintainer" assumes the person deciding in Freeside maintains the target
repository. That holds for the operator who installed the App.

Revisit when Freeside publishes to a repository whose maintainers are not
the operator, or when the human gate becomes the default.
