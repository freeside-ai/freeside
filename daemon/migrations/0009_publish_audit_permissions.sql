-- The live WorkflowAudit producer (#182) expands the installation token's
-- minimum repository permissions. Keep requested and granted values lossless
-- in the existing mint audit, just as 0006 did for the publication scopes.
ALTER TABLE publish_mint_audits ADD COLUMN requested_actions TEXT NOT NULL DEFAULT '';
ALTER TABLE publish_mint_audits ADD COLUMN requested_administration TEXT NOT NULL DEFAULT '';
ALTER TABLE publish_mint_audits ADD COLUMN requested_environments TEXT NOT NULL DEFAULT '';
ALTER TABLE publish_mint_audits ADD COLUMN granted_actions TEXT NOT NULL DEFAULT '';
ALTER TABLE publish_mint_audits ADD COLUMN granted_administration TEXT NOT NULL DEFAULT '';
ALTER TABLE publish_mint_audits ADD COLUMN granted_environments TEXT NOT NULL DEFAULT '';
