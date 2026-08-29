# Correlate Conversation Reads Before Trust

Context: issue #693 and PR #1009.

Chose to re-run the daemon's conversation collection invariants on every
`getConversation` response, as well as correlating its conversation ID with
the request, before applying the snapshot or certifying the decision card.
A decoded response can still name an unrelated conversation or carry foreign
message ownership, duplicate identities or attachments, empty identities or
attachments, a missing creation time, or noncontiguous sequences, so the
generated response type is not sufficient authority for cache identity or
thread validity.
Centralizing the gate in the API contract layer keeps bootstrap, cache restore,
card validation, successful discussion refreshes, direct conflicts, and replay
conflicts on the same fail-closed path. Every conflict carrying a conversation
ID refreshes that thread, even when the rejected action was not `discuss`;
only the presentation
of an awaiting-agent conflict remains action-specific. Replay conflicts also
derive the awaiting-agent state from that correlated thread before re-enabling
any action.

Also chose a sandwich read for conversation-bearing item state: read the item,
read its correlated conversation, then confirm the item resource version and
decision bindings did not change. Revision equality alone was rejected because
unrelated daemon work may advance the global frontier; stable item versions
prove the paired thread was observed while those bindings were current. A
changed confirming item retries or revalidates instead of certifying a torn
pair. The same helper covers validation, successful discussion refreshes,
direct conflicts, and replay conflicts.

The refute-first pass injected decoded responses with the wrong conversation
ID, foreign message ownership, duplicate message IDs, noncontiguous sequences,
empty message IDs and attachment digests, and duplicate attachment digests.
The pass also supplied nonpositive snapshot versions, Go's zero message
timestamp, and a conversation revision beyond the enclosing bootstrap or cache
cursor. The client rejected each response, including a malformed bootstrap
collection, kept it out of the cache, and left actions disabled. A relaunch
also rejects a decoded cache containing a malformed or future conversation. It also
committed an agent completion between the
item and conversation reads; the client rejected the stale item bindings and
retried to a coherent pair. A source sweep confirmed that every production
`getConversation` call uses the correlated, stable-pair helper, and that
bootstrap and cache relaunch use the same contract gate.

Chose a post-object `getSyncRevision` frontier as the revision ceiling for
every direct conversation read. A structurally valid direct response is not
canonical: it could claim a future revision and advance the observed cursor
before a heartbeat exposes the real frontier. The client compares the
frontier's epoch with the active sync cursor, observes its revision, and then
uses that revision as the ceiling. Observing the accepted frontier before the
remaining pair checks preserves the highest observed server revision when the
pair is discarded. `getSyncBootstrap` was rejected because downloading every
synchronized row adds no authority beyond the same epoch and revision
envelope; the mock's revision read is side-effect free. Using either item read
as the ceiling was also rejected: the first would reject the legitimate race
where an agent completes between reads, while the confirming row is itself an
untrusted returned object and could claim a future revision. The adversarial
pass returned a thread beyond the canonical server frontier, confirmed it
never entered the cache, and proved the next heartbeat remained healthy. A
source sweep confirmed that the stable-pair helper is the only production
direct `getConversation` caller.

The owner kept this PR on its declared conversation scope. The same audit found
pre-existing unbounded revision ingress for attention items, conflict
replacements, command results, runs, and timelines, but centralizing that gate
would add unrelated revision consumers and violate this work unit's explicit
non-goal. Follow-up: #1015 records the full class and its adversarial acceptance
cases.

The mock restore helper now rewinds its conversation table together with the
attention frontier. Leaving a later thread behind was rejected because mock
bootstrap would otherwise combine pre-restore item rows with post-restore
conversation rows, unlike the daemon's transactional restore.

Also chose to clear a discussion's pending-command ledger after a valid 200
`CommandResult`, matching every other non-snooze action. The earlier design held
the ledger until the refreshed thread contained the exact user-message receipt.
That treated a definitive result as commit ambiguity and could present Retry for
a command the daemon had already committed. The client still refetches the
conversation after submit so the message can render before the next heartbeat.
If that auxiliary read fails, the card returns to idle, revalidates, and lets a
later heartbeat or bootstrap deliver the thread. A genuinely lost response
still replays the identical `command_id` idempotently.

Also chose separate observation paths for resource mutations and bare server
revisions. This preserves same-revision conversation writes across relaunch
without rewriting the whole cache for unchanged periodic heartbeats. Rejected
unconditional same-revision persistence because it made an idle heartbeat and
bootstrap perform redundant full-cache writes.

Revisit the sandwich read when the daemon exposes a bound item-and-conversation
read or another contract that rules out torn pairs. Today the daemon's 409
binding check already rejects a decision made against such a torn pair, so the
sandwich is an additional read-time guard. Revisit the structural checks when
the generated client enforces the full conversation invariants and
request-to-response identity; today a daemon bug in any checked message fails
the whole card closed. Revisit cache observation when persistence becomes
transactional or diff-based.
