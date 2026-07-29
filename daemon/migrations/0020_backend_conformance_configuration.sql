-- Bind every new backend-conformance proof to the exact normalized runtime
-- configuration the suite exercised. Existing rows predate that identity;
-- retain them as append-only audit history under the reserved unbound digest,
-- which domain/store gates refuse as admission or restoration authority.
ALTER TABLE backend_conformance_records
ADD COLUMN configuration_digest TEXT NOT NULL
    DEFAULT 'sha256:0000000000000000000000000000000000000000000000000000000000000000'
    CHECK (configuration_digest <> '');
