-- The source file's digest is identity, not a derivable rendering of decoded
-- publication metadata, so retries retain it durably.
ALTER TABLE production_attempts ADD COLUMN publication_digest TEXT;
