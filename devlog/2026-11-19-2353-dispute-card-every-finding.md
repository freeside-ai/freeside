# Show Every Finding of a Disputed Round and Hold Its Settled Outcomes

The owner decided in #3436 that a round which raises a dispute card shows
every finding on it, and that nothing settled in that round is recorded or
replied on GitHub until the person answers the card. Chose holding settled
declines and defers over recording them before the card, because on #2382's
exit run a decline settled itself, posted "Freeside declined this finding" on
a finding the reviewer had labeled blocking, and the card never showed it.

This revises [per-finding adjudication](2026-10-14-1140-per-finding-adjudication.md)
(#1990) for rounds that raise a card. That note recorded accepted declines
and defers before raising the dispute so their replies went out at once, and
rejected listing the whole batch because the card would ask again about
findings already replied to. Holding those outcomes removes that objection:
nothing has been replied to yet, so the card can list them. Rounds that raise
no card keep #1990's timing. Each answer is still independent, and the
remediation hold from that note is unchanged.

## One Rule Decides What Waits for a Person

`domain.FindingNeedsPerson` decides which findings wait, and
`domain.SettledDisputeRouteIn` derives a row's `settled_route` from the
latest model-authored adjudication. The engine uses both to build the card,
and the store re-derives a stored `settled_route` with the same function on
write and on read and refuses a mismatch. A settled route is an authority
bit: omitting a settled row from `finding_routes` records that route and
replies with Freeside's attribution, so it must never be trusted as stored.

The store accepts a row stored with no `settled_route` even when the rule
would settle it. Chose that over requiring equality because a null grants
nothing: the row must be answered, which is the old behavior. Requiring
equality would break cards raised before the field existed and fallback
cards, whose rows are all answered by design. The plan comment asked for a
strict match; this is a departure the PR records.

An omitted row keeps its route only when it carries a `settled_route`, and
the store checks that the kept adjudication entry and route equal what the
card showed. An answered settled row is an ordinary human choice and replies
as "A maintainer ...".

## A Reviewer's Blocking Label Holds the Finding

An external finding whose comment opens with a bracketed badge containing
the token `blocking`, such as `[P3, blocking]`, waits for a person like a P0
or P1, on every round, carded or not. Chose reading the badge at decision
time over storing it as a finding field, which would have been a schema and
import change for one token. The parser reads only the first 64 bytes of a
leading `[...]` badge, splits it into tokens on any character other than a
letter, digit, hyphen, or underscore, so `[P3 blocking]` and `[P3 | blocking]`
match while `non-blocking` does not, compares case-insensitively, and ignores
every other label. A reviewer who writes the label in a second badge,
in parentheses, or elsewhere in the comment is not held; that miss is
accepted, since the contract names a leading bracketed badge and the format
is the reviewer's own convention.

Revisit when an external reviewer's labels gain a structured source, such as
a review API field, that could replace parsing the comment text.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong before commit.

- Confirmed and fixed: the parser first split the badge only on commas, so
  `[P3 blocking]` missed a badge the contract covers. It now splits on any
  separator, as above.
- Confirmed and fixed: an earlier build committed a round's settled
  outcomes, and so their replies, before putting its card, in a separate
  transaction. A daemon that crashed between the two and restarted on this
  build would list those findings as settled, and an override would be
  silently dropped because the outcome already exists. A settled finding
  whose outcome is recorded now stays off the card, as it did before.
- Allowed by decision: a second badge or a parenthesized one is not read,
  per the paragraph above.
- Disproved by trace: a forged or stale `settled_route`, an omitted
  must-answer row, a reply while the card is open, and breakage of cards
  already open. The store gate runs on write and on every read; the signet
  and the store both refuse an unanswered row without a settled route;
  replies follow only recorded outcomes, which the card path no longer writes
  before the answer; and an open card keeps its rows, which have no settled
  route and so must all be answered.
