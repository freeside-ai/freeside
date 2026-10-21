# Attention-Item Conclusion

Issue #2097. A closed attention card now says what happened to it, from a
`conclusion` the daemon stamps on the item when it closes and the sync read
carries to the client.

## Decisions

- **Folded the contract with its first consumer.** The owner chose one
  `kind:contract` unit over a contract-first split on 2026-10-21. The card is
  the field's only consumer, so the schema and the card are verified together
  by one `scripts/check.sh` run over `api`, `daemon`, and `app`; a split would
  have merged a field nothing read and reviewed its shape without the screen
  that justifies it. The unit is deliberately over the size budget for that
  reason (issue Scope).
- **One `conclusion` object over loose fields on the item.** Chose a nullable
  object with `cause`, `concluded_at`, `reason`, `successor_item_id`, and
  `decision` over more top-level item fields, because the cases are exclusive:
  a `decision` cause carries the command (action, device, display name,
  finding routes, answer route) and an empty reason; every daemon cause
  (`expired`, `superseded`, `resolved_by_daemon`) carries a reason and no
  command. The object makes that pairing one validated value. Null means open,
  or closed before this version; the client keeps the old status sentence for
  the second case rather than inventing a cause.
- **The typed route list is the contract, not the message.** `decisionMessage`
  still folds finding routes into the thread message; the conclusion carries
  them typed, because the card outlines the applied route per finding and
  parsing prose for it would make the message a contract.
- **The decider's name is read at decision time.** `concludeItem` reads the
  device's display name in the deciding transaction and fails the command when
  the device is missing, rather than writing an empty name or resolving it on
  read, because a later rename or unpair would otherwise rewrite history.
  The unattended label-intake initiator (`daemon-label-intake`) has no device
  record, so its decisions name the fixed "Label intake".
- **Every close is gated, not just the new ones.** The store refuses a
  transition to a closed status without a conclusion whose cause matches, and
  refuses an item created closed without one. Chose fail-closed over a
  default cause, so a close site missed now, or added later (#2111), fails a
  test instead of persisting a card that cannot say why it closed.
- **Task record v4.** `freesided inspect task` prints each item's
  conclusion, which changes the record's JSON shape, so its version moves
  from v3 to v4 by the record's own rule.

## Owner Design Decisions (2026-10-21)

1. **Applied route in the ink selection ring, not green.** "Applied" records
   a choice, not a success, and `tokens.css` reserves green for diffs (R28).
   The FAVORED mark stays on the adjudicator's lean.
2. **One OUTCOME fact group** on decided and daemon-closed cards alike.
3. **The drawn frames are the defaults:** facts as rows, not one sentence
   under the lead (alternative A); every route drawn inert, not the applied
   route alone with the rest folded (B).
4. **"Open task timeline" under the facts,** not as the first fold (C). The
   timeline tells what followed (remediation, re-review, publication); the
   card does not redraw it (#2100).

Applying them showed one rule the frames left implicit: a closed card loses
its controls, not its content. On `ready_for_final_review` the change summary
sits between the reviewing action and the actions, so it still draws when the
control group does not.

## Refute-First Findings (Store Re-Gate)

A fresh-context reviewer tried to break the store gate and the close sites.

- **Allowed by decision: a closed write that omits the conclusion keeps the
  stored one.** Several writers rebuild a closed item from constructors and
  replay it; restoring the decoded conclusion (which decode already
  validated) lets that replay converge instead of reading as an erasure.
  Chose restore over refusing such replays because the conclusion is
  immutable, so the only value a closed write could legitimately carry is
  the stored one. The restore applies to closed writes only, so a stale
  pre-close open copy still fails as stale (`TestAttentionItemConclusionGate`).
- **Disproved by checks:** erasing or substituting a recorded conclusion
  (immutability check); closing, or creating closed, without one; a stale
  open write over a closed row; a planted row whose conclusion disagrees
  with its status or `decided_at` (decode fails closed); a daemon cause with
  `decided_at`, or a decision cause without it; a supersession without a
  successor, or a successor on another cause.
- **Confirmed and fixed: the reattempt close put operator text in the
  conclusion.** The task record prints the conclusion's reason on the claim
  that the daemon owns every byte of it, and an operator's long reattempt
  reason would also have exceeded the 1,024-byte cap and failed the
  reattempt. The conclusion now names only the attempt; the item's reason
  keeps the operator's text, as before. Every other close site writes fixed
  sentences or identifiers the daemon owns.
- **Confirmed and fixed on the card:** a closed card kept no Retry for a
  command whose response was lost, and a closed specification thread still
  offered a reply. Retry now sits under the uncertain receipt, since
  resending is how the device learns whether the command landed; the reply
  is gone.
- **Declined:** cross-checking a decision's action against the status, or
  its routes against the item's findings, in `validateConclusion` (the
  daemon is the only writer and decode re-validates structure); stamping
  close times from an injected clock at sites that take none and already
  use wall time for their other writes.

## Revisit When

- A second consumer of `conclusion` appears (the task timeline, a
  notification), since its needs may change the shape.
- The task timeline absorbs the closed card's job (#2100), which may make the
  Outcome facts redundant.
