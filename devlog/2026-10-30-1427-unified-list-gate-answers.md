# Unified List Gate Answers

The owner answered the three questions the 30 Oct unified-list handoff
(`docs/design/handoff-2026-10-30-unified-list/`) left open before its
first unit could start. The README's ◆ Gate carries the same answers
beside each item; this note keeps the reasons.

## More Than One Open Item Per Task

Chose to keep the daemon's current behaviour, a task may hold several open
attention items, over a one-per-task rule where the newest supersedes the
older. The daemon already allows it (`ListOpenAttentionItemRecordsForTask`
returns a list and nothing makes an open item unique per task), so the
client has to render what the daemon can send, and a rule would be daemon
work outside a presentation-only handoff. The task page therefore builds
the Decisions chip and popup, the one-line item rows and the "N open
decisions" status line (#2445). The owner decided on 2026-10-30.

Revisit when a task with several open items proves confusing in use; then
a one-per-task rule is a daemon unit, and the client drops those three
elements.

## Section Names and Items Without a Task (◆4, ◆6)

Chose the handoff's drawn defaults, Needs you / Working / Finished, and a
row in Needs you for a project- or system-scoped item (system-scoped rows
survive the project filter; a closed one rows in Finished) over the listed
alternatives, because the list can't be built without them and nothing
argued against the drawing. The owner delegated the choice on 2026-10-30.

## Snoozed Proposals (◆2)

Deferred. The handoff asked whether the client holds a snoozed proposal's
snooze-until; it doesn't. The daemon drops a snoozed item from the sync
snapshot (`daemon/internal/signet/sync.go`) and no sync field carries the
instant, so the "Back at" context line needs a contract unit. The owner
chose not to decide snooze presentation now, so no contract issue is filed:
a snoozed proposal leaves the list while snoozed, as today, and its task
rows under Working by its stage.

Revisit when snooze is used enough that a task vanishing from Needs you
without a visible return time is a problem.

## Left Open

◆1 (a multi-item row's count), ◆3 (pinning) and ◆5 (the outline row's
trailing slot). Each default is "build nothing", so none blocks a unit.
