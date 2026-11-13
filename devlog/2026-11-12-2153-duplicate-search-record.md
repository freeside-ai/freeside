# A Follow-Up Filing Waits on a Recorded Duplicate Search

Issue #3146. Tier A (the record and the engine step decide whether a
finding is filed and which issue a verdict names), so a mandatory note.

Chose a separate duplicate-search record, read by the filing gate, over a
field on the filing proposal, because a duplicate answer produces no
proposal at all, and its evidence still has to exist for the comment that
#3147 will propose on the chosen issue.

## Decision

- **A record per finding, keyed like a filing source.** The record is
  keyed by adjudication digest and finding, the same pair a filing source
  carries, so the gate reads it directly at admission and at approval. A
  proposal field would exist only when a filing is proposed and would
  change the digest of every existing proposal's encoding.
- **The site sits under the finding-adjudicator role.** Plan §5.13 keeps
  the role list closed, and the search judges the same finding the
  adjudicator just set aside. It is still its own site with its own choose
  contract, as plan §7 requires, not the adjudicator's annotation site.
- **Handles, never numbers.** The site sees `candidate-N` handles, titles,
  and excerpts. The client refuses an answer naming a handle it wasn't
  shown, and the record's validation refuses a `duplicate_of` outside its
  own candidates on every read. Only an issue the daemon listed can be
  named.
- **No search, no filing.** A search that can't run (no lister or site, a
  failed listing, or an unusable answer) raises the run's existing filing
  notice with class `duplicate_search_unavailable`, and the entry gets no
  card. A card without a search would let an operator approve a possible
  duplicate, which the plan rules out.
- **The notice ends searching for the run.** A parked run re-enters on
  every reconcile; retrying would list GitHub and call the model each pass.
  Once the notice exists, no later pass searches. The cost is that one
  transient listing failure ends filings for that run.
- **100 candidates, most recently updated first.** One GitHub page, no
  pagination. The record carries `truncated` when more open issues exist,
  so a reader knows older issues weren't considered. The run's own bound
  issue is left out, since the run is closing it.
- **The answer is taken as given.** A duplicate verdict drops the filing at
  any confidence. No gating on confidence until misfires show it's needed.
- **Titles are made single-line.** The lister replaces control characters
  in titles with spaces, so a stored title never carries a line break, and
  cuts it at 256 bytes on a rune boundary.

## Consequences

- A duplicate verdict is invisible to the operator until #3147 proposes a
  comment on the chosen issue; until then the record is the only trace.
- The approval gate now refuses a filing card admitted before this change,
  since it has no record. No live deputy run has filed yet, so none should
  exist; an operator holding one declines it.

## Refute-First Findings

A fresh-context reviewer tried to break the change. It found no path that
admits or approves an unsearched filing, or names an issue the daemon
didn't list. Each finding it raised is allowed by decision:

- **Open pull requests share the listing page.** GitHub's issue listing
  includes pull requests, which are dropped after the one GET. A target
  with about 100 recently updated open pull requests leaves few or no
  issues to judge, and an empty set records "new" with no site call.
  Allowed: this repository had 1 open pull request and 393 open issues
  when the unit landed. The search API would avoid it, but its index lags
  issue creation.
- **Escaped issue text can pass the 512 KiB input cap.** The candidates
  field is JSON inside the input's JSON, so characters that escape grow up
  to about sevenfold. Around 60 crafted issues push a search past the cap.
  Allowed: the result is a site fallback, which refuses the filing and
  raises the notice, the outcome the plan's risk section accepted.
- **The notice is run-wide.** One failed search ends searching for later
  rounds of the run too, and the notice keeps whichever class came first.
  This is the decision above.
- **A rename disables the search until the project record follows.**
  Membership is checked against the project's stored name, so a renamed
  repository fails every listing closed until the rename is recorded.

Revisit when false duplicates drop real follow-ups (gate on confidence),
when transient listing failures leave runs without filings often enough to
need a retry or an operator retry action, or when a target repository
carries enough open pull requests to crowd issues out of the listing.
