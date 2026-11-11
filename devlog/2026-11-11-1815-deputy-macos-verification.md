# The Deputy Names the macOS Executor in an Optional Settings Key

Issue #2978, deferred from #2882
(`devlog/2026-11-09-2207-verification-room-selection.md`). Tier A: the
section configures the executor whose conformance pass the deputy proves at
start, so it decides which root, slots, and toolchain pin that proof covers.

Chose one optional `macos_verification` object in the deputy settings file
over a new settings format version, because an optional key keeps every
existing file loading with its bytes, and so its settings revision,
unchanged. The `agent_tree.second_writer` key set the precedent (#2653).

## Decision

- **Optional key, no version bump.** The key is a pointer tagged
  `omitempty`, so `requireEveryKey` lets it be absent and requires all four
  inner keys when it is present. The format stays
  `freeside-deputy-settings-v1`. A bump would have made every operator
  rewrite a file that names no executor, for no change in meaning.
- **Absent is the only way to say "none".** An absent object configures no
  executor, as an empty `-macverify-root` does. A present object with an
  empty `root`, `xcode_build`, or `swift`, or no slots, is refused rather
  than read as "none". Otherwise a half-written object would quietly disable
  the executor, and every `macos_host` command would fail at verification
  instead of at load.
- **No slot default.** The flag defaults to `fvslot1,fvslot2`; the settings
  file has no defaults, because each value lands in a record that should
  attest only what the operator wrote. The operator lists the slots the
  install created.
- **Toolchain formats stay in `macverify`.** The settings refuse an empty
  token but not a malformed one; `macverify.New` refuses that at daemon
  start, as it does for the flags. Copying its patterns would give two
  definitions to keep in step. The cost: a bad token is refused after the
  lease rather than before it, and the next start recovers the lease.
- **Tier A placement.** The section's type, validation, and mapping to
  `daemoncompose.MacverifyExecutorConfig` live in
  `cmd/freesided/macverify_executor.go`, already on the Tier A list as the
  flags' parser, so `deputy_settings.go` holds only the field and one call.
  The list needs no new entry.
- **Full pass at every start.** The deputy always runs conformance, so a
  configured executor runs `Suite.Full` at each start whose self-check
  passes, and a failed pass
  fails the start, as under `freesided -run-conformance`. The pre-lease
  check and the enabled driver build their configuration through one
  function, and a test compares the two with the section set.

## Refute-First Pass

A fresh-context reviewer tried to prove the change wrong.

- **Confirmed, allowed:** a case-variant null (`"MACOS_VERIFICATION":
  null`) loads as absent. The strict decoder matches member names without
  regard to case, while `requireEveryKey` looks the key up exactly, so it
  skips the variant as an omitted optional key. `agent_tree.second_writer`
  and `writers.specifier_escalated` already behave the same way. Left as is
  because it fails safe: absent configures no executor, so every
  `macos_host` command fails by name and nothing is granted. A required key
  in another case is still refused, and a case-variant object is refused by
  its empty inner values.
- **Confirmed, allowed:** slot names aren't trimmed, unlike the flags'
  `SplitNonEmpty`. A padded or odd slot name fails the conformance pass, and
  so the start, rather than proving a different executor.
- **Disproved:** the pre-lease check and the enabled driver can't differ,
  since both build the section through `deputyClaudeDriverConfig` from the
  same loaded settings, with the slots copied. A present object can't load
  as unconfigured: `null`, `{}`, a missing or null inner key, an empty root,
  wrong types, an extra inner key, and duplicate keys are each refused.
  An absent key leaves the bytes, and so the revision, unchanged. Unsorted
  slots reach the same configuration digest as the flags', because
  composition sorts them.

Revisit when: the deputy needs an executor it should not prove at every
start, or a second optional section would make a format version cheaper
than another omitempty key.
