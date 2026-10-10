# Phone Notifications Link Into the App; the Sender Waits on the Engine

Work unit: #1924 (lane signet, kind:feature). Scope: `daemon/`, `app/`,
`devlog/`. No shared package changes.

## The Tap Link Is `freeside://`, Not the Daemon's Address

Revises `2026-07-16-2038-opened-receipt-contract.md`, which pointed the
notification's Click at the item's URL on the daemon's API origin
(`ClickBaseURL`) and rejected a custom URL scheme because "the app has no URL
handling yet, and the daemon must not depend on it". Owner decision, settled
when #1924 was planned and handed to implementation (2026-10-10).

Chose `freeside://attention/items/<id>?channel=ntfy&attempt=<n>` (and
`freeside://inbox` for the test notice) because the old reason no longer
holds and the old link cannot work: this unit gives the iPhone app link
handling, and an `http://` link to the API opens a browser that holds no
device credential. The path and query are unchanged, so the app still derives
the exact receipt PUT from the link. `NtfyConfig.ClickBaseURL` is gone, and
with it the daemon's address no longer reaches the provider.

- Rejected: a universal link. It needs a hosted domain and an associated-
  domains entitlement that free provisioning does not have.
- Rejected: keeping the `http://` link beside the new one. ntfy carries one
  Click target.

The provider-visible metadata is otherwise what the 2026-07-16-2038 note
accepted: item ID, channel name, attempt number, priority.

## What a Link May Do

Any app or web page on the phone can open a `freeside://` link, so the link
is untrusted input. Chose a parser that accepts exactly the two forms the
daemon writes and refuses everything else, and a handler that only navigates
and reports a receipt. A link never carries an action.

Refute-first findings (`docs/agent-workflow.md`):

- **A forged link cannot act on an item.** Confirmed by the parser's refusal
  table: an extra path segment, an unknown or repeated query name, a
  non-decimal attempt, userinfo, a port, or a fragment all parse to nothing.
- **A forged link could aim the receipt at another path.** Was true in
  form: an item ID or channel of `.` or `..` decodes into the receipt's
  request path, where a URL loader resolves it. Fixed in the parser, which
  also refuses a control character, a percent-encoded or upper-case host,
  an empty port, and an attempt with a leading zero: none is a form the
  daemon writes.
- **A forged link cannot report another device's receipt.** Disproved by a
  check: the receipt PUT is authenticated as this device, and another
  device's attempt answers 404. The most a forged link does is mark this
  device's own attempt opened and show a card.
- **The topic reaches a log or an error.** Was true: `net/http` quotes the
  request URL, which ends in the topic, in every transport error. Fixed at
  the source, in `publish`, so every caller gets a status code or a fixed
  failure class. Tests pin it for the sender, the test notice, the
  daemon log, and `notify-test`'s output.
- **A redirect carries the topic to another host.** Was true: the publish
  client followed redirects, and `net/http` sends the previous URL, which
  ends in the topic, as the `Referer` of the next request, with the access
  token on the same site. A followed 301, 302, or 303 also turned the POST
  into a GET, so a sign-in page's 200 was recorded as the provider's
  acceptance. Fixed in `publish`: no redirect is followed, and a 3xx is a
  rejection with its status. A test pins it for all five redirect statuses.
- **The topic on screen.** Allowed by decision: the phone's own Devices card
  shows its server and topic, because the operator has to subscribe to it.
  No other device's card does, and the Mac shows none.
- **A copied topic syncs to the operator's other Apple devices.** Allowed:
  they are the same owner's devices, and the copy control is the only way to
  move a 35-character topic into the ntfy app.

## The Sender's Rules

Settled at planning; recorded here so they do not live only in the issue.

- **The ntfy app receives the notification, not Freeside.** An iPhone app
  cannot listen while closed without APNs, which Freeside lacks before
  Phase 2.
- **Each open item notifies each active device once**, whatever its
  interruption class. Not notified: an item no longer open, a snoozed
  proposal, and an item created before the device paired.
- **Six attempts, then stop**, on waits of 1, 2, 4, 8, and 16 minutes. The
  delivery rows are the sender's only memory, so a restart resumes where
  they say it stopped. A crash between the publish and the acceptance write
  sends one duplicate a minute later; accepted.
- **Every active device's topic gets the notification**, including a Mac
  nobody subscribed. `Device` has no platform or opt-in field, and adding one
  is a contract change.
- **The test notice is `freesided notify-test`** over the host control
  socket, because only the daemon holds the key that derives topics. It
  records no delivery row.

## The Sender Does Not Start in `freesided` Yet

Finding (agent, 2026-10-10): an accepted notification, like an opened
receipt, raises its item's `item_version` (2026-07-16-1718 note). The engine
binds pending work to the exact version. With the sender running in the
command, `TestDaemonRecoversAcrossSIGKILL` restarts with a pending discuss
intent, the notification is accepted first, and the engine's binding check
fails into a durable stop.

The 2026-07-16-1718 note owned one consequence of that version movement: a
client's prepared command meets a 409 and converges. The plan for #1924
carried the same view. Neither considered engine records, which have no
replacement path. Until this unit no production code wrote a delivery row,
so the case could not arise.

Chose to build and test the sender and leave it off in the command, over
starting it with the hazard or adjusting the kill-recovery test's
expectations. The interval is opt-in configuration
(`config.AttentionDeliveryInterval`), which the in-process test daemons need
anyway: they default to the hosted ntfy server.

Follow-up: #1946.

Revisit when: #1946 merges (start the sender in the command and count the
delivery's version movements in the kill-recovery test), a second delivery
channel lands, or APNs arrives in Phase 2 (the link forms and the receipt
path should carry over unchanged).
