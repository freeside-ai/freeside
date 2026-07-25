# Keystore: Name The Unreadable Record, Don't Skip It

Issue: #271. Chain context: `devlog/2026-07-24-1637-wave2-chain-amendment.md`.

One pre-#245 record on the maintainer's machine (owner-keyed, but with null
`owner`, `owner_id`, `visibility`, `key_id`, and `name`) made
`Keystore.ListApps` fail with `registration owner is empty`, which fail-closed
every `InstallationResolver.Resolve` and every `InstallationJanitor` cycle: one
unusable record denied all credential operation, and the error named nothing an
operator could act on.

## Decisions

- **Chose keeping enumeration fail-closed and naming the offending record over
  skipping it.** Owner resolution selects among *all* local registrations and
  refuses more than one match; a view that silently omitted one record could
  bind an owner through a second registration while the omitted one bound it
  too. The janitor has the same requirement from the other side: it cannot claim
  complete coverage while one registration is invisible to it, and coverage is
  what gates minting. So the blast radius is the bug, not the refusal.
  `ListApps` now returns `*UnreadableRegistrationError` carrying the record's
  numeric owner key and wrapping both `ErrUnreadableRegistration` and the
  underlying cause.
- **Chose multi-error unwrap over a flat sentinel** so callers that already
  distinguish a cause keep working: a widened-permissions record still matches
  `ErrCredentialPermissions` and still produces the doctor's
  `bad_keystore_permissions` finding, while a record that fails only on identity
  falls through to the new `unreadable_registration` finding carrying its owner
  ID. The finding deliberately carries numeric coordinates only, like every
  other credential finding.
- **Chose an explicit `QuarantineApp` withdrawal over in-place repair as the
  operator's remedy.** Repair would return the registration to janitor coverage,
  and the janitor deletes installations absent from its authority snapshot, so
  "fixing" a record the operator no longer manages is the destructive direction.
  Quarantine is not deletion: the key and metadata move intact to a sibling
  directory inside the same credentials root, staying within the containment
  boundary that keeps App keys out of checkpoints and workspaces. It refuses a
  readable registration (withdrawing a working one is fail-open), refuses to
  overwrite an existing quarantine record, and moves the `.staging`/`.old` swap
  journals with the active directory so the next enumeration cannot promote the
  record back.

## Rejected Alternatives

- **Return the valid registrations plus a rejected set, and let consumers decide.**
  Rejected: every current consumer needs the complete set to be correct, so each
  would have to re-derive the same refusal, and the first one that forgot would
  fail open silently.
- **Repair the record in place from the App's canonical GitHub metadata.**
  Rejected for the janitor-coverage reason above, and because it would make the
  keystore contact the forge during a local load.
- **Extend `MigrateLegacyApp` to cover this shape.** Rejected: that path exists
  for the pre-owner-keyed *singleton* layout and requires the operator to supply
  the missing attribution; this record is already owner-keyed, and the operator's
  intent here is withdrawal, not attribution.

## Refute-First Findings

The withdrawal path moves credential directories, so it took the destructive-path
pass. It took the harness form rather than delegated lenses: the failure mode is
a disagreement between two code paths over which records are usable, which a
state-space enumeration measures and a diff-read can only assert.

- **Confirmed (raised by review, P1): enumeration and quarantine disagreed about
  readability.** A record whose metadata is internally valid but whose persisted
  owner ID does not bind to its directory key is rejected by enumeration and was
  accepted as "readable" by quarantine, so the doctor routed the operator to a
  remedy that refused the record. Both paths now share one gate,
  `loadRegistrationAt`, which is the only place readability is decided.
- **Confirmed (raised by review, P2): a record stuck in recovery was
  unreachable.** With no active directory and an invalid `.old` journal,
  `recoverSwap` fails, which blocked enumeration and made quarantine exit before
  it could collect the journal. Withdrawal now tolerates a failed recovery,
  because an invalid journal is one of the states it exists to remedy.
- **Confirmed by extension: recovery failures were unattributed.** A failed
  `recoverSwap` is a per-record state that blocks enumeration and whose remedy is
  withdrawal, so it left the same nameless error the issue is about and now
  carries its owner ID.
- **Corrected (raised by review, round 2): a leftover-clearing failure is not a
  readability failure.** An over-wide first sweep attributed it too, which put
  it on the withdrawal path while quarantine still judged the record readable.
  The disagreement is real, but the right resolution is the narrower
  classification, not the wider gate: the registration loaded and bound, so
  withdrawing it would drop a working registration out of janitor coverage over
  a stale journal, and the cleanup error already names the leftover path.
  Enumeration returns it unattributed again, and readability now means exactly
  load-plus-binding in both paths.
- **Confirmed (raised by review, round 2): the withdrawal persisted its source
  removal before its destination entry.** For a cross-directory rename that
  makes a crash between the two syncs lose the credential outright. The
  destination is now persisted first, so the only crash-side divergence is a
  stale source entry that still holds the record.
- **Confirmed (raised by review, round 3): the owner ID was dropped by the
  finding codes that matched first.** A record present but missing its key wraps
  `ErrNoAppCredentials`, which the doctor's switch matched before the sentinel,
  reporting `missing_machine_key` with owner zero; a per-record permissions
  failure had the same defect uncited. The owner is now extracted once, before
  the switch, and rides every finding code, since the specific code is more
  useful to the operator than collapsing all three into
  `unreadable_registration`. A keystore-wide failure still names no owner.
- **Rejected by verification: the crash-side divergence wedges the remedy.**
  A retry meeting an existing destination refuses rather than overwriting, and
  that refusal is correct rather than a wedge: POSIX forbids a second directory
  entry for one directory inode, so a state with both present is two distinct
  records, not one interrupted move, and silently replacing the preserved one
  would be the credential loss the ordering exists to prevent.
- **Confirmed (raised by review, round 4): a keystore-wide failure read as a
  damaged record.** `assertPermissionsAt` asserts the credentials root and the
  registration root as well as the record's own directory, so a widened mode on
  either failed the load gate for every registration alike and let withdrawal
  proceed against whichever owner was named — dropping a working registration
  out of janitor coverage over a directory mode, while leaving enumeration just
  as blocked. Withdrawal now rejects those two modes before it reaches any
  record, matching the order enumeration already uses.
- **Confirmed (raised by review, round 17), and the class closed structurally.**
  A malformed destination — a file or symlink where the owner's target
  directory belongs — passed every directory-mode check and still made the
  rename impossible. That was the seventh member of one family: attribution
  re-derived the withdrawal's preconditions instead of asking it. Each earlier
  round widened the derivation (permissive modes, owner access, the directory
  set, the predicate), and each time a precondition present in one side and
  absent from the other produced another member. Attribution now runs the
  withdrawal's own preflight, `withdrawalUnavailable`, which is exactly what
  `QuarantineApp` checks before it mutates anything, so a new precondition is
  inherited by both by construction. Two preconditions attribution had never
  applied came along for free: a malformed target, and a destination whose
  source still exists, which is a distinct earlier withdrawal the remedy
  refuses.
- **Confirmed (raised by review, round 16): the two sides of the gate used
  different predicates.** Splitting the check into a helper for attribution and
  one for withdrawal left attribution testing only owner access, so a
  too-permissive quarantine destination was accepted there and rejected by the
  withdrawal. Enumeration never sees that asymmetry on the two roots, which it
  mode-checks itself, so the destination was the only place it showed. Both
  sides now call one per-directory gate, which is the point: a predicate that
  accepts what the withdrawal rejects recreates the defect the gate exists to
  prevent. The mode table gained the permissive direction, which exercises all
  three tables at once.
- **Confirmed (raised by review, round 15): the mode gate covered the sources and
  not the destination.** An existing quarantine directory the owner cannot write
  into fails every rename exactly as a deficient source root does, yet the gate
  listed only the two roots, so the record was attributed and the remedy refused.
  Rounds 4, 11, 12, 13 and 15 are one class with one statement: every directory
  the withdrawal operates through must permit what it does there. The directory
  list is now derived in one place, `withdrawalDirs`, whose comment says that a
  directory missing from it is precisely this defect, and the mode table runs
  over the destination as well as the roots.
- **Confirmed (raised by review, round 14): the crash barrier was conditioned on
  the wrong thing.** Round 9 persisted the journal moves only when an active
  record was also moving. With no active directory and two journals — an invalid
  `.old` blocking recovery and a valid `.staging` — the barrier was skipped, so a
  crash could persist the second removal and not the first, leaving recovery a
  promotable stage and resurrecting the withdrawn record. The condition was never
  the active record: it is that a journal on the source side is
  promotion-capable. Each journal removal is now persisted before the next
  rename, which also subsumes the barrier the active record needed.
- **Confirmed (raised by review, round 13): the keystore-wide gate was applied
  per-branch instead of per-policy.** Round 12 put it on the recovery-failure
  attribution and not on the ordinary load-failure attribution, so a damaged
  active record under an unwritable root was still attributed and still routed to
  a remedy that refuses. Every attribution now passes through one function whose
  contract is the promise it makes: name an owner only where withdrawal is
  actually available. The predicate was right; its placement was the miss.
- **Confirmed (raised by review, round 12): the root-mode class recurred a third
  time, so the gate moved to the invariant.** Owner-write alone is not what the
  remedy needs: a directory without owner search can have its names listed but
  not be traversed or renamed within, so a `0600` root passed the write check and
  still broke both recovery and withdrawal. Rounds 4, 11 and 12 were three
  members of one class — the roots must permit everything the remedy performs —
  which the fix now states directly, requiring all three owner bits, with the
  mode space enumerated as a table (no write, no search, neither) rather than
  the member that happened to be reported. Withdrawal also checks the roots
  before anything else, since an unsearchable credentials root cannot be
  evaluated for legacy layout or records at all.
- **Confirmed (raised by review, round 11): the keystore-wide class recurred
  from the opposite direction.** Round 4 rejected roots that are too permissive,
  using `assertMode`, which only inspects group and other bits. A root the owner
  cannot write passes it, yet blocks the rename recovery and withdrawal both
  need, for every record alike — so a record reachable only as a journal was
  attributed as unreadable and routed to a remedy that fails against the same
  unwritable parent. `narrowDir` deliberately preserves a tighter-than-0700
  root, so this is a supported state rather than corruption. Both roots are now
  checked for owner-write, and the failure is classified keystore-wide in both
  paths. The first sweep was one-directional; the class is the mode of the roots,
  not the direction of the deviation.
- **Rejected by verification: quarantine can be used to drop a working
  registration.** A record that loads and binds is refused, and the refusal is
  now judged by the same gate enumeration uses, so the two cannot drift apart
  again.
- **Confirmed (raised by review, round 9): the move order was sound against
  errors and not against crashes.** Moving the active directory last makes every
  *error* partial state safe, because the failing rename simply does not happen.
  A crash is different: with no persist between the journal moves and the active
  move, the active rename can reach disk while a journal rename has not, and
  recovery then promotes the surviving journal and resurrects the registration
  the withdrawal promised to remove. The round-5 ordering argument silently
  assumed a durability it never established. A barrier now persists the journal
  moves before the active directory leaves, which makes that promotion
  impossible whatever happens to the active rename.
- **Confirmed (raised by review, round 7): a sync failure left the withdrawal
  unfinishable.** With every rename landed and a directory sync failing, nothing
  remained to move, so a retry reported a missing record and never re-issued the
  sync. The suggested remedy of persisting resumable state does not reach this
  boundary: a journal write carries the same barrier that just failed, so it
  cannot make durability recoverable. What is tractable is idempotent
  completion, so an owner whose record is already in quarantine re-issues both
  syncs and reports success. That is also the honest contract for a remedy: the
  postcondition it promises, withdrawn and preserved, holds.
- **Confirmed (raised by review, round 6): the cleanup class recurred through a
  second call site.** Recovery clears the journals itself after promoting a
  record, so a cleanup failure there was still labelled unreadable even though
  the promoted registration loads fine and withdrawal would refuse it. The
  round-2 sweep had narrowed only the outer call site, which is a miss, not
  convergence. Attribution is now decided by the invariant it actually claims —
  whether the record loads after recovery, and therefore whether withdrawal can
  help — rather than by which error the failure carries, so a third call site
  cannot reintroduce the class.
- **Confirmed (raised by review, round 5): a multi-directory withdrawal could
  strand its own record.** A damaged registration with a swap journal moves more
  than one directory, so a failure part-way left the record split. The wedge is
  not the split itself: with the active directory gone and a journal left,
  recovery promotes that journal into the active slot, and the retry then
  collides with the record its own earlier attempt had already quarantined.
  Ordering closes it rather than rollback, which can fail in turn: journals move
  first and the active directory last, so any partial state keeps the active
  directory present, recovery has nothing to promote, and the retry finds free
  destinations for whatever remains. A piece already moved is absent from the
  source list entirely, so its destination is never re-examined.
- **Rejected by verification: withdrawal can be made to destroy a preserved
  record.** A second withdrawal for the same owner is refused rather than
  overwriting the first, and an incomplete first-save stage is discarded by
  recovery rather than withdrawn, so quarantine reports nothing to withdraw.
- **Rejected by verification: a withdrawn record can be resurrected.** The
  `.staging` and `.old` journals move with the active directory, so the next
  enumeration cannot promote the record back into the active set.

## Deferred, With The Tension Named

Withdrawing a journal-only record leaves `<owner>.old` with no `<owner>`, so a
later generation for the same owner can be withdrawn beside it and the preserved
pieces stop being attributable to a key. Refusing whenever any owner suffix
already exists would fix that and break the resumption property above: the
resumed partial withdrawal is the same on-disk state, journals in quarantine
with the active record still on the source side, and the two are
indistinguishable from the filesystem alone. Separating generations therefore
needs a withdrawal identity, which needs somewhere to record which withdrawal is
in flight — persistence with no consumer today. Deferred to #280, whose
acceptance carries both resumption properties so the fix cannot regress them.
`QuarantineApp` has no non-test caller yet, so the layout stays free to change.

## Verification Gap

The crash barriers between renames are verified by inspection of the call order,
not by a harness: the daemon has no crash or fsync-failure injection at this
layer, and adding an injectable sync purely to observe them would put test
scaffolding in the credential path. The fixtures pin what is observable without
injection — the move order, the resumption of a partial withdrawal, and that no
source-side piece survives for recovery to promote.

One residual window is irreducible without a withdrawal manifest: the final
rename has no persist after it until the closing sync, so a crash inside it can
return that one directory, and if it is a promotable journal recovery may adopt
it. Per-rename persistence bounds the exposure to that single in-flight rename
rather than to any earlier one; closing it entirely needs the explicit resumable
state #280 would introduce.

## Accepted By Decision

`.DS_Store` and other unexpected entries inside the registration root still fail
enumeration closed (`unexpected registration entry`, surfaced as the doctor's
`bad_keystore_permissions`). Refusing an unexpected entry in a credential store
is the correct posture, and the failure is loud and names the entry in its error
text; giving it its own finding code is a diagnostics refinement that belongs
with the doctor packaging work, not here.

## Revisit When

`freesided onboard`/`doctor` (#238) gains an operator-facing repair path: at that
point quarantine becomes one branch of a remedy menu, and the finding should
route to it rather than to a library call.
