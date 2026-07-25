# Installation Authority From A State-Directory Snapshot

Issues: #276, #231 (chain), #281 and #283 (follow-ons). Prior note:
`devlog/2026-07-24-1637-wave2-chain-amendment.md`, which recorded the owner
choice this unit implements.

The janitor gate refuses every registration that has not completed a pass, and
minting resolves through it, so `ErrJanitorInactive` was the daemon's answer to
every credential request. Both ports the janitor needs had test implementations
only. This unit supplies them from two files in the daemon state directory.

## Decisions

- **A missing authority is an error, never an empty one.** The zero
  `InstallationAuthority` is not a safe default: `reconcileRegistration`
  classifies every observed installation of a registration with no bindings as
  unbound and deletes it. So an absent file, an unreadable one, and a
  registration the document does not name all deny the pass. The same reasoning
  makes an omitted `trusted_installations` key an error rather than an empty
  set, and makes a pending envelope's `installation_id` a pointer: zero is that
  field's *widest* value (it matches any installation on the expected account),
  so an omitted key would be the one place in the format where absence widened
  authority.
- **Every key must be authored; only `null` says "deliberately absent".** Go
  decodes an absent key and an authored null into the same zero value, so
  wherever the zero value is the *more permissive* reading, a typo silently
  chooses it. Most fields are safe by accident (an absent ID is zero and zero is
  rejected), but the exceptions are the ones that drive deletions: an absent
  `pending` leaves a fresh native install unprotected, and an absent
  `trusted_installations` trusts nothing. Rather than enumerate which fields are
  dangerous and re-litigate that list as the format grows, every key in both
  files is required.
- **Duplicate JSON object keys are refused.** `encoding/json` resolves a
  repeated key silently, last scalar or array winning and nested objects
  merging, and neither `DisallowUnknownFields` nor field validation can see it
  because the collapse happens first. In a file the design expects operators to
  hand-edit, the surviving value is routinely the narrower one, and narrower
  here means deletions.
- **The quarantine set is stored, not derived from the audit entries.** It is
  bounded by the number of installations ever quarantined, while the audit log
  only grows, so an operator can rotate the log for size without silently
  restoring trust. The `action` is written by whichever recorder method was
  called, so the set never depends on the janitor's internal mapping from
  removal reason to destructive action.
- **The journal's read-modify-write takes an advisory lock on the state
  directory.** `NewInstallationJanitor` takes the authority and the recorder as
  separate parameters, so a composer can build one store per port; the
  in-process mutex belongs to one value and cannot serialize that. Measured
  before the fix: two stores, 40 quarantines each, 38 silently lost. A lost
  withdrawal re-trusts an installation the daemon has already suspended and
  deleted.
- **The journal's size bound is enforced on write.** Its audit entries carry the
  repository set observed at grant drift, sized by the account being reconciled
  (the untrusted side of this boundary, capped at 10,000 IDs by the janitor's
  own pagination limit). A journal written past the size it can be read at would
  deny every registration and could not be repaired by the daemon itself.
- **Subtraction refuses one case rather than resolving it.** Several of the
  snapshot's cross-binding rules are satisfiable by *removal*, so dropping a
  quarantined binding could turn a document the janitor rejected into one it
  accepts. Validating the document as authored closes that for every rule the
  document can see; for the remaining ambiguity, a registration that still binds
  a quarantined installation while carrying a pending envelope denies the pass
  until the operator reconciles the file.
- **`daemon/internal/publish`, not `daemon/cmd/freesided`**, per the chain
  amendment note: the lane that owns the ports owns their first implementation,
  and it keeps this unit's declared paths disjoint from #236's.

## Rejected Alternatives

- **Refusing the pass whenever the operator's file still names a quarantined
  installation**, rather than subtracting. Simpler and strictly fail-closed, but
  it makes any drift on a trusted installation deny that registration until a
  human edits a file, and the janitor already errors when a trusted installation
  is absent remotely, so a quarantine-and-delete would brick the registration by
  construction. It also hands anyone who can change an installation's repository
  selection a denial of the whole publish path.
- **Serving an empty authority when the snapshot has no entry for a
  registration.** Rejected: that is an instruction to delete every installation
  the registration has. The cost of the alternative is #281 (below).
- **Deriving the quarantine set from the audit entries.** Rejected: it ties the
  set's survival to the log's, so rotating the log for size would silently
  restore trust.
- **A `kind:contract` store-backed authority now.** Already rejected in the
  chain amendment note for #263's reasons; unchanged here.

## Refute-First Findings (Acceptance 5)

Three independent lenses, each prompted to disprove the design: destructive
path, adversarial input enumeration, durability and concurrency.

**Confirmed and fixed**

1. An omitted or null `trusted_installations` key, and a duplicated one, decoded
   as "trust nothing" with no error, so a hand-edit could delete every
   installation of a registration. Demonstrated.
2. Duplicate JSON keys anywhere in the document silently narrowed the authority,
   including a merged pending envelope neither authored stanza contained
   (a stale envelope's expiry replaced by a live one's). Demonstrated.
3. An omitted `pending.installation_id` decoded to zero, the widest envelope.
4. The journal had no write-side size bound: six grant-drift records carrying
   10,000 observed repository IDs each pushed it past the 1 MiB read cap, after
   which the store denied every registration and could not write again.
   Measured at 1,142,317 bytes.
5. Two store values over one directory lost withdrawals to interleaved
   read-modify-writes. Measured: 38 of 80 lost.

**Rejected by verification** (not to be re-raised)

- Cross-registration quarantine bleed: `applyQuarantine` filters on the
  registration.
- Quarantine widening through a shared account or a shared repository ID: the
  authored document rejects both, so no surviving binding can inherit them.
- A pending envelope with a positive installation ID protecting a third party,
  or a zero-ID envelope being dropped by another installation's quarantine.
- Login case-folding divergence between the document, the subtraction, and the
  janitor: `validateOwnerLogin` admits ASCII only, so `ToLower` and `EqualFold`
  agree on every input that reaches them.
- Unicode laundering (invalid UTF-8, lone surrogates, homoglyphs, zero-width,
  NFD), numeric edges (floats, exponents, int64 overflow, strings-as-numbers),
  RFC3339 edges (leap second, missing zone, zero time expressed three ways),
  and 20,000-deep nesting: all rejected.
- The superset claim over the *app-independent* rules: a differential run over
  200,000 generated entries found no case the document accepted and
  `validateInstallationAuthority` rejected for an app-independent reason, and
  160,544 subtraction runs left every served entry passing both validators.
- Temp-file cleanup on every `writeFile` error path, including after a
  successful rename; rename atomicity; the pre-effect ordering in `janitor.go`;
  the descriptor-based (not path-based) kind, mode, and size checks.

**Confirmed and fixed in review**

6. An omitted `pending` key decoded to the same nil pointer as an authored
   `null`, so forgetting it left a fresh native installation unprotected and the
   janitor deleted it (Codex P1 on #282). It recurred from an incomplete fix of
   finding 1: presence was required for `trusted_installations` and
   `pending.installation_id` but not for the class. Closed by enumerating both
   files' full shapes rather than by adding the cited field.

**Accepted by decision**

- **The superset claim does not cover app-dependent rules.** Whether a bound
  account is trusted, and whether a public registration's own owner is in its
  trusted set, are decided only on the served value, so subtraction can turn a
  document those rules would have rejected into one they accept. It cannot widen
  what the served authority grants, since removal only ever grants less; what
  changes is that the pass proceeds instead of freezing against a file the
  operator has not reconciled. The doc comment now states this bound rather than
  the unqualified claim.
- **Deleting the journal restores trust** in every installation the operator's
  file still names, and a quarantine set member with no audit entry is accepted
  (that is what makes log rotation safe). A withdrawal survives a restart; it
  does not survive an operator discarding the daemon's state. Documented, not
  defended: the directory is the operator's.
- **Directory ownership is not checked**, only its mode, matching the keystore's
  posture on the credentials root next to it. An attacker who can write to an
  ancestor of the state directory can substitute it.
- **The crash window between the audit barrier and the suspend** leaves the
  installation deleted but never suspended on the following pass, since the
  withdrawal is already durable and the installation returns as unknown. Tested
  and documented rather than closed; closing it would need the suspend to be
  idempotent against a record rather than a decision.
- **`Entries` grows without bound.** The write-side cap turns the failure into a
  denial an operator can repair by trimming entries and keeping the quarantine
  set, which the format's validation permits. Automatic retention is not in this
  unit.
- **A quarantined installation can still match a *later* zero-ID pending
  envelope** (Codex P1 on #282, filed as #283). The withdrawal reaches the
  envelope by installation ID, but a zero-ID envelope names no installation and
  matches by account, so once the operator has removed the stale binding, a
  quarantined installation that outlived its deletion can satisfy a fresh
  native-install envelope and escape cleanup. Accepted here rather than fixed:
  closing it means carrying the withdrawn set into the reconciliation gate,
  which is #263's pending-envelope semantics and an explicit non-goal of this
  unit. Bounded meanwhile: a zero-ID envelope carries an empty current set, only
  that set enters coverage, so the escape grants no repository and ends when the
  envelope expires. Blanket-denying a zero-ID envelope on any account with a
  prior quarantine was rejected, since the quarantine set never shrinks and the
  denial would land on the re-install the operator needs.
- **The entry's epoch and revision must be positive even with no pending
  envelope.** Stricter than the janitor, which reads them only for the frontier
  comparison. Kept: a zero frontier reads as "unset" while silently deciding
  whether an envelope is current.

## Revisit When

`freesided onboard` (#238) owns authoring: the snapshot's contract that every
keystore registration must have an entry becomes onboarding's obligation to
author the entry *before* completing a registration, and the blast radius that
makes that obligation load-bearing is #281. When #265 schedules the movable
control plane, this file becomes the migration's source rather than the
authority.
