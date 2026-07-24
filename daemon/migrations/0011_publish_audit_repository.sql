-- Bind every new installation-token mint audit to the stable numeric GitHub
-- repository ID that the reviewed trust profile authorized (#261). Existing
-- rows retain 0 as an explicit legacy-unknown sentinel; reconstructing an ID
-- from the current owner/name would turn mutable display state into history.
-- The store write path rejects 0 for every new row.

ALTER TABLE publish_mint_audits
    ADD COLUMN repository_id INTEGER NOT NULL DEFAULT 0
    CHECK (repository_id >= 0);
