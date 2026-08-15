# Mint Ntfy Topic Keys At Store Birth

Work unit #521 (PR #807) changes the credential boundary for the persisted
ntfy topic capability, so this note records why every production store opener
now enters through one birth path and the refute-first result required for a
credential surface.

## Store Birth Owns Key Creation

Chose one `openStoreWithTopicKey` boundary over special-casing onboarding or
letting each command sequence key and database creation independently. The
topic key becomes durable before `store.Open` can create SQLite: an open
failure can leave only an owner-private orphan key, which a retry reuses
byte-for-byte, while a database already present without its key remains a
fail-closed loss signal.

Rejected minting after `store.Open`, because a crash in that ordering would
durably manufacture the same existing-store-without-key state that #521
repairs. Rejected minting on first submit, because onboarding and every other
store creator must establish the credential invariant when the store is born,
not rely on one later command to repair it.

## Credential-Boundary Refute-First Result

A fresh-context adversarial pass confirmed that key creation precedes SQLite,
a failed open leaves no database and reuses the same key on retry, a stable
preexisting database without its key fails closed, existing store options are
preserved, and key file type, link, size, and privacy checks fail closed.

The first post-fix automated review disproved the original call-site proof:
`freesided follow` reached `store.Open` indirectly through
`internal/observe/observedb`, outside the command-directory-only AST scan, and
could therefore create a keyless database. Chose one daemon-internal
`topicstore.Open` boundary over duplicating topic-key preflight in the follow
shim, because parsing `-db` twice would drift from the observation command and
would leave two store-birth sequences. The follow database wrapper now uses
the same boundary, its absent-path regression test requires a 32-byte `0600`
key, and the call-site guard walks every production Go file under `daemon/`,
resolves aliased imports of `internal/store`, and permits `store.Open` only in
`internal/topicstore` plus explicitly reviewed exemptions.

The next review disproved the first exemption classification: the manually
invoked `freeside-project-image` binary records its durable result in the
operator's daemon store, so an absent `-db` was another real store-birth path,
not a separate utility database. The root cause was classifying by command
name instead of following the documented data owner. Routed that command
through `topicstore.Open` and added an absent-path credential regression. The
only remaining direct opener is `freeside-signet-dev`, an explicitly non-product
convergence harness with its own configurable or ephemeral topic-key state
machine and a store that the product daemon never reopens; folding it into the
product boundary would erase that test fixture's deliberately different
restart semantics.

A later automated pass found a separate publication failure: creating the
final key pathname before writing and syncing its bytes could leave a partial
credential after a short write or interrupted volume, so the next retry would
reject the orphan as malformed. Chose the existing
`atomicfile.WriteFileNoReplace` primitive over another hand-built write loop:
it stages and syncs the complete `0600` file, atomically renames without
clobbering a racer, and syncs the directory. An injected publication-failure
test proves neither the key pathname nor SQLite exists after failure and that
the same store-birth call succeeds on retry.

Rejected-by-verification: a database-path symlink or same-user path replacement
does not create a new privilege or credential-disclosure path. The database
path and its sibling key are operator-owned local state, same-user mutation is
already inside that trust boundary, and the daemon and submit paths already
used the same stat/key/open sequence before this change. Accepted-by-decision:
the helper retains that lexical path binding and non-atomic filesystem
sequence rather than expanding #521 into a store-identity contract.

Rejected-by-verification: accepting an existing owner-only regular key whose
owner bits are stricter or include an irrelevant execute bit does not widen
credential readability. Creation still writes exactly `0600`; loading rejects
all group or other access, symlinks, non-regular files, hard links, and malformed
lengths.

## Revisit When

Revisit the database/key identity boundary if database paths become writable
by a principal other than the local operator, or if Freeside supports
uncoordinated concurrent store creation outside the daemon lock.
