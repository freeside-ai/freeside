-- Execution hosts (issue #2162).
--
-- execution_hosts holds each host that runs this daemon's cordoned launches
-- (plan §5.7 Execution Hosts): its operator name, the account its runs
-- execute as, how the daemon reaches it, and the operator's override of its
-- cordoned count. execution_host_conditions holds one row per host and
-- condition it is in, with when the condition began, so each unit that
-- observes a condition sets or clears its own row without rewriting the
-- others. domain.ExecutionHost.Validate guards both tables on write and
-- decode, so the condition column carries no CHECK list
-- (devlog/2026-11-07-1113-enum-columns-go-validation.md).
--
-- The daemon's own host is a row from this migration on, with the fixed id
-- and name 'daemon' (domain.DaemonHostID): it runs as the daemon owner,
-- whose account name a migration can't read, and has no reach.
--
-- A plain create and one seeded row, with no rebuild. A client reads the
-- hosts, so the store writes both tables under a revision bump.
CREATE TABLE execution_hosts (
    id                       TEXT    NOT NULL PRIMARY KEY CHECK (id <> ''),
    name                     TEXT    NOT NULL UNIQUE CHECK (name <> ''),
    run_account_name         TEXT    CHECK (run_account_name IS NULL OR run_account_name <> ''),
    run_account_daemon_owner INTEGER NOT NULL CHECK (run_account_daemon_owner IN (0, 1)),
    reach_address            TEXT    CHECK (reach_address IS NULL OR reach_address <> ''),
    reach_host_key           TEXT    CHECK (reach_host_key IS NULL OR reach_host_key <> ''),
    cordoned_count_override  INTEGER CHECK (cordoned_count_override IS NULL OR cordoned_count_override >= 1),
    CHECK ((reach_address IS NULL) = (reach_host_key IS NULL))
) STRICT;

CREATE TABLE execution_host_conditions (
    host_id   TEXT NOT NULL REFERENCES execution_hosts (id),
    condition TEXT NOT NULL CHECK (condition <> ''),
    since     TEXT NOT NULL CHECK (since <> ''),
    PRIMARY KEY (host_id, condition)
) STRICT;

INSERT INTO execution_hosts (id, name, run_account_daemon_owner) VALUES ('daemon', 'daemon', 1);
