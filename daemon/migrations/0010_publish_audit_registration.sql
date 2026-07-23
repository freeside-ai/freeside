-- Bind every new installation-token mint audit to the stable numeric App ID
-- of the registration that authenticated it (#247). Existing rows predate
-- multi-registration minting and retain 0 as an explicit legacy-unknown
-- sentinel; the store write path rejects 0 for every new row.

ALTER TABLE publish_mint_audits
    ADD COLUMN registration_id INTEGER NOT NULL DEFAULT 0
    CHECK (registration_id >= 0);
