-- Task line provenance (plan §5.4, routing policy, revisions 99 and 103;
-- issue #2391): how the router chose the agent of a line it wrote.
--
-- provenance is the domain.TaskLineProvenance JSON: the routing policy
-- revision applied, the selection trigger, the reading acted on, and the
-- agent passed over. It is set exactly for a router-sourced line, so every
-- row stored before this migration (an operator source) satisfies the
-- CHECK with NULL. The provenance is bound into the row's content address,
-- and the store re-derives the id from the decoded value on every read, so an
-- edit that changes any decoded field fails closed as a corrupt row. An edit
-- to the JSON's form alone (key order or case, an unknown key) decodes to the
-- same value and reads as the line it was.
ALTER TABLE task_lines ADD COLUMN provenance TEXT
    CHECK ((source = 'router') = (provenance IS NOT NULL));
