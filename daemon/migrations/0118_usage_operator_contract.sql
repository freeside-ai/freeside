-- The client contract for usage and the operator settings beside it (#1719;
-- plan §5.4, §5.14).
--
-- A pool's usage record is now projected to clients, so every write to it is
-- a client-visible transaction: it stamps the revision it commits at and
-- advances its own version, as every synced row does. No production writer
-- existed before this migration, so the defaults never meet a real row.
ALTER TABLE pool_usage_observations ADD COLUMN as_of_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pool_usage_observations ADD COLUMN entity_version INTEGER NOT NULL DEFAULT 0;

-- One receipt per accepted refresh_usage, reassign_collector,
-- change_task_line, or set_routing_policy command, keyed by its command_id,
-- which is unique across command kinds. body is the kind's record
-- (domain.CommandReceipt); as_of_revision is the revision of the transaction
-- that accepted it, which a replay returns unchanged.
CREATE TABLE client_command_receipts (
    command_id     TEXT    NOT NULL PRIMARY KEY CHECK (command_id <> ''),
    kind           TEXT    NOT NULL CHECK (kind <> ''),
    as_of_revision INTEGER NOT NULL CHECK (as_of_revision > 0),
    body           TEXT    NOT NULL CHECK (body <> '')
) STRICT;
