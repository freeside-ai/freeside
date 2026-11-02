-- The client contract for usage and the operator settings beside it (#1719;
-- plan §5.4, §5.14).
--
-- A pool's usage record is now projected to clients, so every write to it is
-- a client-visible transaction: it stamps the revision it commits at and
-- advances its own version, as every synced row does. No production writer
-- existed before this migration, so the defaults never meet a real row.
ALTER TABLE pool_usage_observations ADD COLUMN as_of_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pool_usage_observations ADD COLUMN entity_version INTEGER NOT NULL DEFAULT 0;

