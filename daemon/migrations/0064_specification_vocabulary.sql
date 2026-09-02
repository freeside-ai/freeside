-- #986 renamed the elaboration stage to specification. The column follows
-- the vocabulary. The run identifiers it holds keep their bytes: a run minted
-- before the rename stays run-elaboration-<hex> with its elaborate- stage and
-- inv-elaborate- invocations, and the store derives that family from the run
-- ID prefix on read. No row content changes here, and no other table spells
-- the old name outside JSON bodies, which the store canonicalizes on decode.
ALTER TABLE production_attempts RENAME COLUMN elaboration_run_id TO specification_run_id;
