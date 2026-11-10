-- Launch wait keepalive (issue #2953): the latest time a running production
-- publication pass kept alive a wait refused recently enough that its lane may
-- still be pursuing it. The pass is serial, so a launch it waits on is
-- refused again only when the pass next reaches its task, which one long
-- verification can push past the wait's lease. The memory gate counts a wait
-- as live while its latest refusal or keepalive is inside the lease.
--
-- refused_at stays the latest real refusal: it decides which waits the next
-- pass keeps, and the engine throttles refusal writes by it, so a keepalive
-- must never move it. NULL means no pass has kept the wait. It only adds a
-- column and rewrites no rows.

ALTER TABLE launch_waits ADD COLUMN kept_at TEXT CHECK (kept_at IS NULL OR kept_at <> '');
