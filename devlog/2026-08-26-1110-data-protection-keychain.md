# Data Protection Keychain Custody

Work unit: [#959](https://github.com/freeside-ai/freeside/issues/959)
(`kind:fix`, `lane:saddle`). This mandatory note records the revised
credential-custody and destructive-cleanup decision.

## Decision

Chose the macOS Data Protection Keychain with a private, provisioned
`$(AppIdentifierPrefix)ai.freeside.app.macos` access group over the legacy
file-based login Keychain. Signed operator packaging now satisfies the revisit
condition recorded in
`devlog/2026-07-16-1030-saddle-cache-pairing.md`: the app has a stable Team ID
and profile-authorized App ID prefix plus bundle ID, so the credential can
retain `AfterFirstUnlock` accessibility without inheriting a rebuilt binary's
changing code requirement. The prefix normally equals the Team ID, but legacy
accounts can retain a distinct bundle-seed prefix; the installer verifies the
profile Team ID independently rather than conflating the two authorities.

Migration is restricted to the exact current deployment-scoped service name.
The ambiguous pre-#782 decoded-path service remains untouched because it cannot
prove which deployment minted its bearer token. A valid Data Protection item
is authoritative; only its absence permits consulting the exact-service legacy
item. Migration copies and verifies the complete credential before deleting
legacy material, and ordinary deletion clears both backends so a stale grant
cannot resurrect a deliberately forgotten identity.

Chose profile-backed Apple Development signing over ad-hoc operator installs.
The installer's final outer-bundle re-sign preserves the Xcode-generated
entitlements and verifies the application identifier, access group, embedded
profile authorization, and signature before replacement. Rejected installing
an apparently healthy ad-hoc app because it cannot be authorized for this
Data Protection Keychain group and would fail credential custody at runtime.

## Refute-First Findings

Confirmed and fixed one signing defect during a real provisioned-build attempt:
passing the selected certificate's full common name to Xcode conflicts with
automatic signing. The build now passes the generic `Apple Development`
selector plus the certificate-derived Team ID; the final post-templating
`codesign` still uses the operator's exact selected identity.

Rejected by tests and inspection: an authoritative corruption or Security
error cannot reach legacy fallback; migration never deletes legacy material
before a byte-identical authoritative read-back; interrupted cleanup remains
loud and retryable; conflicting duplicates cannot override the authoritative
credential; and macOS deletion attempts both backends while retaining the
first real error. Every constructed query retains only the caller-supplied
exact service, so neither decoded-path nor cross-deployment lookup exists.
Errors carry only Security status values, never credential bytes.

Automated review confirmed two cross-platform signing and custody defects.
Because iOS already uses the Data Protection Keychain by default, a query that
omits its selector is not a distinct legacy backend there; all legacy reads and
cleanup are now macOS-only, and a simulated iOS-policy test proves repeated
loads retain the authoritative item. Provisioning profiles may authorize the
app's exact signed group with an `APPIDPREFIX.*` pattern, so installer
verification now accepts that profile pattern while continuing to require the
signed app entitlement itself to equal the exact private group.

A second automated-review pass found that replacement called the public
two-backend delete in authoritative-first order. If legacy ACL cleanup failed,
that could remove the usable Data Protection item before aborting the new
pairing save. Replacement now encodes first and removes legacy material before
touching the authoritative item; a focused failure test proves the old item
survives. Explicit local-identity deletion still attempts both backends and
retains its original fail-loud, retryable behavior.

The third automated-review pass identified the conflation of Team ID with
`ApplicationIdentifierPrefix`. Apple Development profiles for legacy accounts
may carry a distinct bundle-seed prefix, and Xcode expands the Keychain group
with that value. The owner accepted the corrected identity rule: derive the
exact application identifier and private group from the validated profile
prefix plus bundle ID, while requiring the certificate, signed team
entitlement, and profile `TeamIdentifier` to agree independently. Installer
tests now cover both the common equal-prefix case and a distinct legacy prefix.

The fourth automated-review pass found that the installer checked the
iOS-family `application-identifier` key instead of native macOS's
`com.apple.application-identifier`, which would reject every real provisioned
FreesideMac build. A widened refute-first pass confirmed the native key in both
the signed entitlements and profile, and found the same false fixture premise.
It also found that valid profile authorization is an allowlist: a trailing
wildcard may be scoped below the App ID prefix, and a matching Keychain group
need not be its first entry. Verification now checks the native key, accepts
any valid trailing-prefix wildcard and any matching profile group entry, and
independently requires the profile's team entitlement to match the signing
team. Signed application and group values remain exact, with exactly one
signed Keychain group.

At the fifth blocker-sustained fix round, the convergence checkpoint remained
a go because the new defect was reachable and narrow: Xcode's generic
`Apple Development` build selector can choose a different certificate from the
operator-selected identity used for the final seal, even when both certificates
share a Team ID. Team and entitlement equality therefore did not prove the
profile authorized the final signer. Verification now encodes the exact
operator-selected certificate as DER and requires it to match one entry in the
profile's `DeveloperCertificates` allowlist before using that same identity for
the final seal. Deterministic installer cases cover both mismatch rejection and
a match after another authorized certificate. Expiry, device policy, and
unrelated profile hardening remain outside this work unit unless they become a
reachable contract blocker.

The next exact-head pass found that `plutil -extract` treats periods as
key-path separators. Although the installer had switched to native macOS's
dotted entitlement names, it did not escape those dictionary-key periods, so
every valid provisioned build would still fail closed before installation. The
installer now escapes literal periods in signed and profile entitlement-key
components while retaining real separators for the profile's `Entitlements`
dictionary. The command-stand-in parser now accepts only that escaped form, so
the former green-but-blind fixture premise turns red if it regresses.

The following pass exposed a second failed representation assumption in signer
binding: `plutil` emits a profile's certificate data as base64 text, while the
selected certificate is binary DER. The first byte comparison could therefore
never match. Work stopped for an anti-thrash reassessment; a read-only local
profile experiment then proved that OpenSSL's portable base64 decoder
reconstructs a valid DER certificate from the extracted value. The verifier
now performs that decode, fails closed on malformed data, and compares exact
DER bytes. Harness profiles now carry base64 too, including mismatch,
later-match, and malformed cases, so the storage-boundary representation can
no longer be masked. Using the already-required OpenSSL boundary also keeps the
installer harness portable to the required Ubuntu scripts-CI job; Darwin-only
`base64 -D -o` flags would fail there.

The installed macOS SDK's `SecItem.h` confirms that
`kSecUseDataProtectionKeychain` enables access-group and accessibility
semantics on macOS, and that omitting `kSecAttrAccessGroup` selects the first
declared Keychain access group. Installer tests independently reject missing
or mismatched signed entitlements and profile authorization after the final
re-sign, preventing an unprovisioned replacement from appearing healthy.

Accepted verification limit: the real signed smoke reached automatic
provisioning but stopped because the selected development team has not
registered this Mac and has no matching development profile. No install,
credential migration, or developer-account mutation was attempted, and the
implementation did not fall back to legacy or ad-hoc signing. The profile and
prompt-free relaunch observations therefore remain environment-limited for
human verification on a registered Mac.

## Revisit When

The credential becomes intentionally shared across app targets or signing
teams, Apple changes the provisioning requirements for macOS Data Protection
Keychain access, or deployment identity no longer maps one-to-one to the exact
Keychain service.
