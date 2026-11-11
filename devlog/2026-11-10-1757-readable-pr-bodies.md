# Pull Request Bodies a Reviewer Can Read

Issue #3022; plan revision 115. The owner directed on 2026-11-10, during the
Wave 9 exit run, that a body Freeside publishes read like Freeside's own pull
requests: gh-imgup#147 ran to about 90 lines for a 6-line docs change, most
of it digests, invocation ids and a Disposition History written for Freeside,
not for the target repository's maintainer.

Chose a short Verification section and no Disposition History in the body
over the forensic forge copy, because the store already holds the forensic
record and the reviewer reading the body uses none of its digests.

## The Condition It Replaces

Plan §7 recorded an owner condition (#525): at publication the PR carries the
disposition history (review rounds, final dispositions with reasons, the
readiness derivation) "so the merged PR explains itself forensically on the
forge". That condition assumed the forge was where someone would reconstruct
a run. It isn't: the store, the run timeline, and the clients' structured
dispositions carry the same record with authority, and the forge copy was a
rendering of them. The one property the forge copy gave that a reviewer uses
is the declined and deferred findings with their reasons, so those stay.

## Decisions

- **Publisher sections change in place for every record.** Verification and
  the history were never part of the frozen recipe rendering, and earlier
  changes to them (#1553, #2349) applied to every record. An open PR
  published before this change gets the new Verification and loses the
  history the next time the publisher converges it; merged and closed PRs are
  never touched.
- **The intent still binds the history.** `RenderDispositionHistory` keeps
  its exact bytes as the canonical form the publication intent's
  `DispositionHistoryDigest` binds, so recovery of an intent written before
  this change still validates. It is no longer body text.
- **Declined and deferred findings sit in Verification.** The publisher
  can't write into authored sections such as Review Notes, so each finding is
  a labeled-claim sub-bullet under the review bullet. Rejected: giving the
  findings to the author as input, which would turn trusted records into
  agent prose.
- **One closing line replaces the `Not run:` bullets.** The daemon keeps no
  structured record of which checks an agent claimed, so it can't list them.
  "Freeside ran only the checks listed here. The rest of this description is
  its author's account." covers authored and operator prose alike.
- **No digest in the body.** Recipes `freeside.client-publication/v3` and
  `freeside.intake-publication/v2` drop the "Produced by" line, the evidence
  list and the artifact digest; the digest stays on the stored authoring
  artifact. A truncated claim in Verification says it was truncated, without
  the content digest other sections give.
- **The author stops restating verification.** The prompt told the author to
  label its account of verification and review as "reported claims", which
  undersold checks Freeside ran. It now says not to describe them at all;
  Review Notes is for where to look and what the reviewer must decide. This
  is a control-plane change: the operator re-adopts the prompt digest after
  upgrading, and until then the role is unbound and bodies fall back to v1.
- **The source reference moves into Why when that is provably safe.** The
  publisher places its `Closes`/`Refs` block at the end of an opening
  `## Why` section and heads the body otherwise: no opening Why, a fence
  open at the insertion point, or a raw HTML line in the section. The old
  comment rejected any splice because a close keyword inside a code fence
  closes nothing; these fallbacks keep that failure from happening, and a
  placement the rule can't show safe keeps today's position. The owner kept
  this in the same unit rather than splitting it off.

## Refute-First Record

A fresh-context reviewer tried to refute the change before push.

- **Confirmed, fixed: a list item's fence.** A fence indented under a list
  item ends with the list on GitHub, but the line scan read a later
  top-level opener as its closer and placed the reference inside code.
  Placement now falls back on any indented fence opener; a column-0 opener
  is always top-level.
- **Confirmed, fixed: a carriage return.** GitHub ends a line at `\r`, the
  scan didn't, so a closer could go unseen. Placement falls back on any `\r`.
- **Allowed: recovery against a PR merged across the upgrade.** An intent
  written before the upgrade whose PR a human then merged recomposes a
  different body, so recovery stops with a publication conflict. It needs a
  crash between creating the PR and recording the outcome, it fails
  closed, and #1553 and #2349 accepted the same window.
- **Deferred, predates this change.** The authored-prose renderer has the
  same list-fence mis-model when it closes a fence the author left open
  (#3053). Inline links render inside the `<code>` claims Verification now
  shows (#3054).
- **Held.** Every accepted body still fits the 64 KiB limit; recorded strings
  can't forge a marker, heading, close keyword, or mention; the intent's
  history digest is pinned; replaced test assertions track the intended
  change. The fence rules' move kept behavior on two million generated
  lines.

Revisit when a target repository's maintainers ask for the review trail on
the forge, or when a client stops showing structured dispositions.
