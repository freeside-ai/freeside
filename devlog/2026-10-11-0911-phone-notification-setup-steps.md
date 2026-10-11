# The Phone Copies Its Topic and Never Draws It

Work unit: a direct owner assignment (2026-10-11), the app-only part of
shortening phone notification setup. Scope: `app/`, `devlog/`. No shared
package changes. Refs #1924.

## The Topic Is No Longer Drawn

Revises one line of `2026-10-10-1740-phone-notification-links.md`: "The topic
on screen. Allowed by decision: the phone's own Devices card shows its server
and topic, because the operator has to subscribe to it." Agent decision while
rebuilding the card, offered to the owner for veto at review.

Chose to copy the topic without drawing it, over keeping it on the card,
because the stated reason no longer holds. The operator subscribes by pasting,
and **Copy Topic** is how the topic reaches the ntfy app; nobody reads or
retypes 35 characters. A drawn topic adds only exposure: it shows in a
screenshot, a screen share, and a screen recording, and whoever holds it reads
this phone's notifications. The copy control and its sync to the owner's
other Apple devices stay as that note allowed.

The server is still drawn, and only when it is not ntfy's own
`https://ntfy.sh`: the ntfy app subscribes there by default, so another
server is the one value the operator has to enter by hand.

Revisit when the ntfy iPhone app accepts a link that adds a subscription
(its Android app takes `ntfy://`; the iPhone app registers no scheme as of
`binwiederhier/ntfy-ios` on 2026-06-27). Then the topic need not reach the
clipboard either.

## "Notification Opened" Is an Observation

The phone records when it last opened a `freeside://` notification link, one
date per device ID in its own preferences, and the steps show that time or
"Never". Neither value is a secret.

Chose the wording "Notification opened" over "Notifications working" because
the app cannot tell who opened the link. Any app on the phone can open one,
and so can a notification sent before the phone paired again or by another
deployment: the link names no device and no deployment (#1958). The row is
the only sign on the phone that setup reached the end, so it says what was
seen and no more.

Rejected recording only the test notice's `freeside://inbox` link: an item's
notification opening is the same evidence, and an old test notice misleads
the same way.

Revisit when a link names its deployment or device (#1958). Then an open can
be matched to this pairing and the row can claim more.

## The Test Step Stays on the Daemon's Host

Rejected a "send test notification" control on the phone in this unit: the
API has no operation for it, and `api/` changes only through a contract
unit. Follow-up: #1959.
