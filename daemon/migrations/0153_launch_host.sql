-- The host a launch runs on, and the hosts a waiting cordoned launch was
-- considered for (issue #3641).
--
-- execution_admissions gains execution_host_id, the host the admission's
-- launch runs on (plan §5.7 Placement). The store derives it from the
-- admission body's host on every write, and no reader treats it as
-- authority. Every existing admission ran on the daemon's host, which the
-- default records: 'daemon' is domain.DaemonHostID
-- (devlog/2026-11-26-0457-execution-host-contract.md). It has no foreign
-- key to execution_hosts, because SQLite refuses to add a REFERENCES column
-- with a non-NULL default, and the store doesn't check that the host exists
-- until admission chooses one (#2166).
--
-- run_hold_observations gains cordoned_launch, the fact beside a
-- cordoned_launch_cap hold, as JSON: a list of hosts, each with its
-- conditions, has no columns of its own, as quota_alternate_agents doesn't.
-- It is NULL on every other hold and on a cordoned_launch_cap hold recorded
-- before this migration. domain.CordonedLaunchHold.Validate guards it on
-- write and decode.
--
-- Two added columns, with no rebuild and nothing backfilled beyond the
-- default.
ALTER TABLE execution_admissions ADD COLUMN execution_host_id TEXT NOT NULL DEFAULT 'daemon'
    CHECK (execution_host_id <> '');
ALTER TABLE run_hold_observations ADD COLUMN cordoned_launch TEXT
    CHECK (cordoned_launch IS NULL OR cordoned_launch <> '');
