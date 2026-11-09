# Health Signals Count Open Findings and Land in System Health

Unit: #2921. Builds on #2920
(`devlog/2026-11-09-0729-system-health-settings-pane.md`).

## Decision

The owner decided on 2026-11-09 that the iPhone gear badge and the Mac
menu bar's System Health line count open findings. Chose the open count
over the issue's original "not yet acknowledged" count because nothing on
the wire or in the client records an acknowledgement: the daemon records
Acknowledge without changing the item. The open count is also the number
the Settings source list already shows for System Health, so every signal
agrees with the pane. Acknowledging changes nothing; the count drops when a
finding closes.

Rejected:

- **A device-local seen record.** It would disagree between the Mac and
  the iPhone, and the client would hold state the daemon doesn't know.
- **A new acknowledged or seen mark on `AttentionItem`.** A wire and daemon
  change, out of scope for a routing unit.

## The Notice Action Rule

The stopped notice's action always lands in System Health. When the stop
names an item open in the synced inbox, it opens that item's card inside
the pane. That covers both causes, because an operator stop's resume notice
is itself a blocking `system_health` finding. When the stop names no open
item, the action, which was withheld before, reads `Open System Health` and
opens the pane alone. Chose to always offer it over withholding it because
the pane shows the admission state and every open finding, so the operator
always has somewhere to go from a stop.

A notification link for a `system_health` item opens the same card over the
pane. The type comes from the synced store, never the link, so an item the
store doesn't hold yet (a cold start) still opens as an inbox card. Chose
that fallback over fetching the item to learn its type, to keep the link
path free of a second request.

## Revisit When

An acknowledged or seen mark reaches `AttentionItem` on the wire. Then the
badge and the menu line could count unseen findings instead of open ones.
