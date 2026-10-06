-- Device activity (plan §5.14 devices; issue #981): when each paired device
-- last made an authenticated request, for the clients' Devices list.
--
-- device_activity is daemon-internal bookkeeping, deliberately a separate
-- table from devices with no revision columns, like device_credentials: a
-- device row is synchronized state, so recording activity there would bump
-- the server revision and the device's entity_version on a client's own
-- heartbeat and invalidate every cache. The list endpoint joins this table
-- by id; nothing here is carried by sync.
--
-- A device has no row until its first authenticated request after this
-- migration. Nothing is backfilled: the daemon never recorded when a device
-- was last seen, so a missing row reads as "not seen", never as an invented
-- instant.
--
-- last_seen_at is INTEGER UTC unix nanoseconds, like the schedule instants
-- (0025): the refresh compares the stored instant with the request's inside
-- one statement, which RFC 3339 text cannot do (its fractional seconds are
-- trimmed, so text order is not time order).
CREATE TABLE device_activity (
    device_id    TEXT PRIMARY KEY REFERENCES devices (id),
    last_seen_at INTEGER NOT NULL CHECK (last_seen_at > 0)
) STRICT;
