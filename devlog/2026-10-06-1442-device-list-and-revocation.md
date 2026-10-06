# Device List and Revocation

Issue #981 adds `GET /devices` and a Devices screen on the Mac and iPhone
apps, so an operator can see and revoke paired devices. The issue fixed the
contract shape. This note records the choices it left to the implementation,
where the implementation departs from the plan comment, and what the
refute-first pass found.

## The Record

**Last-seen lives outside the synchronized device row.** `last_seen_at` is a
field of the list entry, not of `Device`. A client polls the revision every
few seconds, so an activity write inside the row would move the server
revision on every poll and invalidate every client's cache. The
`device_activity` table carries no revision columns and is written through
`Store.WriteInternal`, which cannot move the revision or an entity version.
The consequence is in the contract: a change to `last_seen_at` is visible
only by reading the list again.

**Chose an integer instant and one conditional upsert over RFC 3339 text and
a read before the write.** The staleness test is `new - stored >= interval`.
RFC 3339 text with a variable fraction does not compare in instant order, so
the column is unix nanoseconds and the comparison runs in SQL. One statement
inserts a first sighting, refreshes a stale one, and leaves a recent one
alone, with no read-then-write window. The same comparison keeps the stored
instant from moving backwards when the clock steps back.

**Chose the `authenticated` middleware over options on
`NewRequestAuthorizer`,** which the plan comment named. The authorizer is a
pure read that answers yes or no. Recording through the service reuses its
clock and logger and leaves the authorizer's signature and every existing
test double alone. A test double that vouches for a device with no row now
reaches the activity write, which the foreign key rejects, and the request
is still served: that is the advisory rule's test.

**Activity is recorded before the handler runs,** so a device that lists the
devices sees its own request instead of a null for the device reading the
screen.

**A failed activity write is logged and the request proceeds.** Last-seen is
advisory, and a store error there must never lock a device out. It is logged
at warn, not dropped, so a persistently failing write is still visible.

**Chose five minutes as the granularity.** It bounds the write rate to one
row write per device per five minutes while still telling a device in use
from one in a drawer. The plan comment marks this as the owner's to veto;
it is one constant, `deviceActivityGranularity`.

**No backfill.** A device paired before migration 0094 has no row and reads
null until its next authenticated request. Writing `paired_at` there would
claim a sighting the daemon never recorded.

**The list is never cached on the client.** `DevicesModel` owns it and reads
it on appear, on Refresh, and when the heartbeat observes a new revision. A
cached list would show a last-seen instant that nothing invalidates, and a
stale list could show a revoked device as able to act. A failed read clears
the rows for the same reason.

**Self-revocation deletes the credential only on the daemon's own answer.**
Deleting the credential cannot be undone, so `DevicesModel.revoke` calls
the sign-out only after a 200 whose snapshot names this device as revoked.
A transport error, another status, or a snapshot naming a different device
leaves the credential in place. When the daemon has revoked the
device but the local delete fails, the model sets the unauthenticated
freshness, so the existing revoked banner's Pair Again is the retry.

**Chose a sheet over a new tab or screen,** opened from a Devices toolbar
button. Device management is occasional, and a sheet matches the New Task
and recovery sheets. One `.sheet` on the synced root serves the Mac toolbar
and both iPhone stacks. The plan comment marks placement as the owner's to
veto.

**Chose the system confirmation dialog for Revoke,** as the revoked banner's
Pair Again uses (#1458), over the `ConsequenceSheet` decision cards use. That
sheet binds a submission to a reviewed item by id and version, which a
device has no counterpart for. Open question 13 in `app/SURFACES.md` still
covers the general ruling.

**The permissive mock seeds three devices; the enforcing mock seeds none.**
The mock app's session is the fixed identity `device-mock`, which never
pairs, so without a seed the list could not contain the device reading it.
An enforcing mock lists exactly what paired with it, as the daemon does, so
the pairing tests keep their counts.

## Refute-First Pass

The device list is a credential-leak surface, and self-revocation is a
destructive path over a returned object, so the change had an independent
review prompted to refute it before the first commit. It found no defect in
the daemon and four in the app, each fixed in the commit that owns it.

**Confirmed: a late revoke answer could delete a newer pairing's
credential.** The sign-out was `AppSession.rePair()`, which deletes whatever
credential the session holds. The operator can close the sheet while a
self-revocation is in flight, see the revoked banner, and pair again before
the delayed 200 arrives; the answer then deleted the new device's
credential. `rePair(endingPairingOf:)` now ends only the pairing the sheet
was opened under, compared by coordinator identity, and is a no-op once the
session has moved on.

**Confirmed: the sheet reopened by itself after pairing again.** The
presentation flag lives on the root navigation model and outlived the synced
surface. A probe with the same view shape showed the sheet presented again
on return. The sign-out now clears the flag once the session has left ready.

**Confirmed: the revocation failure outlived the read it asked for.** The
message tells the operator to refresh and stayed after a successful read. A
successful read now clears it.

**Confirmed: the mock omitted `last_seen_at` for a never-seen device,** where
the contract requires the key and the daemon writes an explicit null. The
generated optional encodes by omission, and the wire test's subset check hid
it. The mock transport now writes the null and the test pins the exact keys.

**Disproved by a check:**

- Credential material in the list. `domain.Device` has five JSON fields,
  neither read touches `device_credentials`, and the Go test pins the exact
  key set and scans the body for the tokens and stored digests.
- Sign-out without the daemon's confirmation. A 503, a thrown error, a 404,
  a 401, and a 200 naming another device each leave the credential in place.
- A device stranded after a real self-revocation whose answer was lost.
  Refresh reads 401, which clears the rows and raises the revoked banner.
- The activity write moving the revision, an entity version, or the stored
  instant backwards, or writing inside the interval.
- The activity write denying or blocking a request. It queues on the single
  connection as the authorizer's read does and adds no new busy class; a
  failed or cancelled write is logged and the handler still runs.

**Allowed:** every authenticated request runs one more internal
transaction, holding one conditional statement. It writes a row only when
the stored instant is at least five minutes old.

## Revisit When

- A consumer needs activity finer than five minutes, or needs an activity
  change to reach clients without a list read.
- The device count grows enough that an unpaginated list is a cost.
- Device renaming or a per-device permission model lands; both are non-goals
  here and would change what a card shows.
