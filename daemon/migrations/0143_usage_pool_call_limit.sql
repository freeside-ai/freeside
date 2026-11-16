-- Each usage pool's judgment call limit per site (issue #3215, plan §5.4).
--
-- A judgment call that finds its site busy now waits its turn instead of
-- returning the site's fail-safe, and how many calls one site runs at once on
-- a pool's credentials is the pool's max_calls_per_site. Every pool starts
-- at one, the number the old one-call-per-site rule fixed, so no deployment's
-- concurrency rises on upgrade. The pool is stored as a JSON body the store
-- validates on read, and a body without the field fails that validation, so
-- this rewrites every existing row's body and changes nothing else. No column
-- is added: nothing reads the pool's extracted columns.
UPDATE usage_pools
SET body = json_set(body, '$.pool.max_calls_per_site', 1);
