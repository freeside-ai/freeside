-- Device pairing credential contract (plan §5.14, issue #64): device records,
-- their daemon-internal credentials, and short-lived pairing codes.
--
-- devices is synchronized state (entity_version/as_of_revision): revocation
-- changes what a device may do, so clients must observe it through sync
-- (§5.14 tests 15-16). The status column is extracted and cross-checked on
-- read; the recorded lifecycle lives in the body.
--
-- device_credentials is daemon-internal verifier material, deliberately a
-- separate table from devices with no revision columns: the synchronized
-- device body carries no credential field, so no sync read path can emit even
-- a hash. Write-once; the credential column holds the sha256 digest of the
-- issued token (credential_hash) or the device public key
-- (device_public_key), never reusable plaintext.
--
-- pairing_codes persists only a keyed digest of the code (HMAC-SHA256 under
-- a daemon-held pairing key that never enters the store; the plaintext is
-- displayed on the daemon host once and discarded). Keyed, not bare: a
-- displayed code is short, so an unsalted fast hash would leave a leaked
-- database or checkpoint offline-brute-forceable while a code is valid. Consumption is the one-way
-- (consumed_at, device_id) pair: the CHECK keeps the pair all-or-nothing, and
-- UNIQUE device_id plus the store's conditional consume (UPDATE ... WHERE
-- device_id IS NULL) make "one device per code" (§5.14 test 14) a constraint,
-- not a convention. Expiry is a comparison against expires_at at redemption
-- (the attention service's job, test 13), not a stored state.
--
-- The pre-existing device_id TEXT columns on commands and attention_deliveries
-- deliberately gain no foreign key to devices: applied migrations are
-- immutable (a rewrite is a hard error in migrate.go), adding an FK in SQLite
-- means a full table rebuild, and §5.14 test 15's "active device" check needs
-- a status read at submission time, which an existence FK cannot provide.

CREATE TABLE devices (
    id             TEXT PRIMARY KEY,
    status         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE device_credentials (
    device_id       TEXT PRIMARY KEY REFERENCES devices (id),
    credential_kind TEXT NOT NULL,
    credential      TEXT NOT NULL
) STRICT;

CREATE TABLE pairing_codes (
    code_hash   TEXT PRIMARY KEY,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    consumed_at TEXT,
    device_id   TEXT UNIQUE REFERENCES devices (id),
    body        TEXT NOT NULL,
    CHECK ((consumed_at IS NULL) = (device_id IS NULL))
) STRICT;
