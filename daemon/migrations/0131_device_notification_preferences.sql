-- Device notification preferences (issue #2931): each paired device's
-- notifications switch. Notifications are hints and the inbox is canonical
-- (plan §4), so a device is notified only once an operator turns it on.
--
-- A separate table, like device_activity (0094), so the devices row and
-- domain.Device stay untouched: a preference grants no authority and has no
-- place in the record the client-authority gates read. Unlike
-- device_activity, the device list reads this row and a client may change
-- it, so every write goes through a revision-bumping Write; the row carries
-- no revision columns of its own.
--
-- A device with no row is off. This migration writes no rows: devices paired
-- before it stay off until an operator turns them on.
--
-- updated_at is when the switch last changed value; while enabled is 1 it is
-- the instant the device was turned on, and the dispatcher owes the device
-- only items created at or after it.
CREATE TABLE device_notification_preferences (
    device_id  TEXT    PRIMARY KEY REFERENCES devices (id),
    enabled    INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    updated_at TEXT    NOT NULL CHECK (updated_at <> '')
) STRICT;
