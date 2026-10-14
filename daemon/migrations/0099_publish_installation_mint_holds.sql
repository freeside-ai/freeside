-- Installation-scope mint holds (issue #2004). One row per installation whose
-- grant-read token outcome the janitor (internal/publish/janitor.go) cannot
-- yet prove harmless: a mint request whose result is unknown, or a minted
-- token whose revoke failed. While a row's held_until is in the future the
-- janitor sends no further mint for that installation, so a daemon restart
-- cannot mint a second token while an earlier one may still be live. #1997
-- kept this hold in memory only; persisting it closes that restart window.
--
-- The janitor writes the row before the mint request leaves the daemon and
-- deletes it only once the token is proven never created (GitHub refused the
-- request) or proven revoked, or once held_until has passed. The row is the
-- authority for whether an installation is held; nothing loads it at startup
-- because the janitor reads it before every mint.
--
-- No token column is present, so no read path can leak a credential: a token
-- retained for revoke retries stays in process memory only, and after a
-- restart the hold simply runs to its deadline. No outcome column either:
-- every row means the same thing, "do not mint before held_until".
--
-- installation_id is the key because GitHub installation IDs are global and
-- the hold is per installation, whichever registration took it; a new mint
-- request replaces the earlier row's deadline. held_until is stored rather
-- than derived from requested_at, so a later change to the token-lifetime
-- constants cannot shorten a hold already taken. Like the mint audit ledger
-- (0033) this is daemon-internal bookkeeping, never synchronized, so it
-- carries no entity_version/as_of_revision columns. installation_id and
-- registration_id reference GitHub-side identifiers, so no foreign keys
-- apply. Timestamps are RFC3339Nano UTC written by Go from the daemon's own
-- clock; the store never relies on SQLite clock functions.

CREATE TABLE publish_installation_mint_holds (
    installation_id INTEGER PRIMARY KEY CHECK (installation_id > 0),
    registration_id INTEGER NOT NULL CHECK (registration_id > 0),
    requested_at    TEXT    NOT NULL CHECK (requested_at <> ''),
    held_until      TEXT    NOT NULL CHECK (held_until <> '')
) STRICT;
