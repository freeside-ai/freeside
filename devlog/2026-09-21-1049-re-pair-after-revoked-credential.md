# In-app re-pair after a revoked credential (#1458)

Chose an operator-confirmed "Pair Again" action on the revoked freshness
banner over an automatic fall-back to pairing on a 401, and left the disk
cache untouched on re-pair.

## Operator confirms; the app never switches on a 401

A 401 does not prove this device's credential was revoked: the wrong daemon
answering at the same `scheme://host:port` returns one too. Deleting a
credential is unrecoverable (the token and ntfy capability exist only in the
grant, then only in the Keychain), so an automatic delete-and-repair would
destroy a still-valid credential whenever a stranger, or a transient
misroute, answered 401. Plan §5.14 also keeps cached items readable while
access is revoked; an automatic switch to the pairing screen would hide them.
So the recovery is operator-driven and gated by a confirmation dialog, and
`AppSession.rePair()` deletes only in `.ready` and fails loud.

A throwing `delete()` is not proof the credential survived. The macOS
`KeychainCredentialStore` deletes the authoritative Data Protection item
before the legacy one and can then throw a legacy-Keychain error, leaving the
authoritative credential already gone. So on a thrown `delete()` `rePair()`
reloads and returns to pairing only on a confirmed absence
(`load() == nil`). A credential that still loads, or a reload that itself
throws, keeps the phase unchanged and surfaces the failure: a load error is
indeterminate (a locked or ACL-restricted Keychain, or the legacy item read
back and re-promoted before a cleanup error, can all leave the credential in
place), so failing loud is safer than showing pairing over a credential that
might still answer. This holds the invariant (never show pairing over a
credential that exists or might; never claim "still paired" over one that is
gone) at `rePair()`, without reordering the store's best-effort two-backend
delete.

## The cache stays; the next sync discards a stale one on its own

Re-pair does not wipe the deployment's disk cache. The cache is scoped to the
deployment, and a re-provisioned daemon reports a new sync epoch, so the first
bootstrap after pairing already discards stale rows (`SyncCoordinator`'s epoch
reset). Wiping on re-pair would add an irreversible step for no gain and would
throw away items that are still the correct deployment's state when the 401
was a transient authz failure rather than a re-provision. Revisit if a stale
row is ever observed to survive the first post-repair bootstrap.

## Known: the action also appears on a Stop 403

`TaskStopModel` maps a 403 to `.unauthenticated`, so the revoked banner (and
its "Pair Again") can appear after a Stop is forbidden, not only after a true
revocation. This is acceptable because the operator, not the app, decides, and
the delete still needs an explicit confirmation. Left as-is rather than
narrowing the banner's trigger, which would be a separate change to the
freshness model.

## Rejected

- **Auto-fall-back to pairing on 401.** Rejected for the reasons above:
  unrecoverable delete on an ambiguous signal, and it hides cached items.
- **Wipe the cache on re-pair.** Rejected: redundant with the epoch reset and
  irreversible.

Revisit when: #981 (device list and revoke) lands and adds its own re-pair or
revoke path, or the freshness model stops collapsing a Stop 403 into
`.unauthenticated`.
