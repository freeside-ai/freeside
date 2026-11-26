# A Paired Phone Sends Itself a Test Notification (#1959)

Work unit: #1959. Scope: `api/`, `daemon/`, `app/`, and this note. A
`kind:contract` unit that carries its first consumer, the phone's control.
Tier A by path: the route's registration line sits in
`daemon/internal/signet/http.go`, on the client-authority list. The handler
and the interval are Tier B by category: the request arrives through the
bearer check and chooses nothing except that a notice goes now to the
caller's own topic.

## Decisions

- **The publish outcome travels in a `200` body; refusals use status
  codes.** The daemon handled the request in every outcome: it asked ntfy,
  or found no notifier. Chose a `200` with an `outcome` over mapping ntfy's
  answer to a `502` or `503`, because a gateway status would let a proxy
  or client retry layer treat the daemon as down and would hide ntfy's own
  status. The refusals the daemon itself makes keep their codes: `403` for
  a device revoked after authentication, `404` for another device's ID (as
  `registerCapabilityContract` answers), and `429` for a call inside the
  interval.
- **A caller may test only its own device.** Unlike
  `PUT /devices/{device_id}/notifications`, where any active device may set
  any device's switch, a test notice goes to the caller's topic only. A
  phone checking its setup has no reason to buzz another phone, and the
  `404` keeps the route from confirming which device IDs exist.
- **The interval is 30 seconds, kept in memory, per device, counting
  publishes.** It bounds a client loop that would otherwise use the daemon
  to flood ntfy. Chose memory over a stored record: the issue forbids a
  table, and a restart that forgets the interval allows one extra notice,
  which is harmless. The slot is taken after the notifier and device checks
  pass and before the publish, so a refused call or a daemon with no
  notifier never blocks a real one, and two concurrent calls cannot both
  publish. A failed publish still counts, because the daemon still tried
  to reach ntfy, and a loop retrying failures is still a loop.
- **`freesided notify-test` stays unlimited.** It runs on the daemon's host
  through the control route, where the operator already holds full
  authority; limiting it would only get in the way of setup.
- **`transport_failure` is the daemon's fixed description, not an enum.**
  It is the failure-class text `ChannelTransportError` already carries
  (such as `request timed out`), shown to the operator as is. An enum would
  turn every new failure class into a contract change for text nothing
  branches on. The body is built from the typed outcome only, never from
  an error's text, so the topic, publish URL, and token cannot reach it.
- **The feature name gates the control.** `device_test_notification` in
  `/health`'s `features` tells a newer app the route exists; against an
  older daemon the phone keeps the host-command step. The change only adds,
  so the compatibility level and floor stay.
- **The phone's control rides in the contract PR** as its first behavioural
  consumer (`docs/coordination.md`, Unit Sizing): a reviewer reads the
  route and the control as one change, and the app's mock-backed tests
  exercise both.

## Refute-First Findings

The route is a credential-leak surface: it is the first place a publish
error can reach an HTTP body. A fresh-context reviewer tried to break it.

- **Disproved by a check:** a topic, publish URL, or token reaching a body,
  header, or log line. Every body is built from the typed outcome or a fixed
  string; `TestHTTPDeviceTestNotificationNeverLeaksSecrets` checks the
  accepted, rejected, and transport-failure cases, and a live call against a
  dev daemon logged nothing about ntfy.
- **Disproved by a trace:** a refused call, or a daemon with no notifier,
  starting the interval; two concurrent calls both publishing. The device and
  notifier checks run before the slot is taken, under one mutex.
- **Disproved by a trace:** a wall-clock step back holding a device in the
  interval. The daemon's clock is `time.Now`, whose monotonic reading keeps
  the elapsed time from going negative.
- **Allowed by decision:** `provider_status` can carry a code outside the
  schema's 100 to 599 when ntfy, or a proxy before it, answers with a
  nonstandard three-digit status, which Go's client accepts. The field is
  only shown to the operator, the app doesn't check the range, and
  `freesided notify-test` prints the same number, so the bounds stay as the
  documentation of what ntfy sends.
- **Allowed by decision:** the mock server doesn't keep the interval itself;
  a test sets the too-recent refusal when it wants one, which is what the
  issue asks of the mock.

Revisit when ntfy gives way to native push, or when a second caller of the
route needs a different interval.
