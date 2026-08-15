# Filter Ambient Metadata Before Ward Seeding

Work unit: #781. This changes a base-identity safety gate and its
host-to-guest trust boundary, so a decision note is mandatory.

## Decision

Chose a narrow host-side staging filter over adding guest-reported path data
to the base proof. Ward excludes only regular files named `.DS_Store`,
`Icon\r`, or with the `._` AppleDouble prefix before snapshot hashing. A
matching directory, symlink, or irregular entry still fails closed, and every
skipped file still spends the seed entry budget.

The same shared staging path serves launch conformance, implement handoff, and
the Codex review lifecycle. Filtering there keeps the private snapshot, its
digest, and the seeded guest workspace symmetric without changing the proof
format or teaching the host to echo attacker-influenceable guest bytes.

Chose a hardened host-local Git preflight for diagnosis. Ward first copies the
checkout without invoking Git and validates and replaces the private
snapshot's repository config with daemon-authored values. It then streams
bounded tracked, status, and ignored-path records against that snapshot under
the daemon's shared Git hardening. Running Git earlier would let a corrupt
checkout execute a configured clean filter as the daemon. The preflight names
the first non-tolerated path as `workspace not clean: <path>` before any VM
launch. Tracked metadata and index promises that can hide bytes
(`assume-unchanged`, `skip-worktree`) are not tolerated. Paths with control,
format, line-separator, paragraph-separator, or invalid UTF-8 bytes are quoted.
The categorical guest-proof error remains the backstop for mutations during
snapshotting and other attestation failures.

## Rejected Alternatives

- **Emit and echo a guest dirty-path proof key.** This expands the
  guest-to-host trust surface for a diagnostic, breaks the pinned never-echo
  rule, and can place attacker-controlled bytes in a failure record.
- **Ignore metadata only in the guest comparison.** The staged snapshot and
  its digest would still carry ambient files, preserving asymmetry between
  Git's commit tree and the writer workspace.
- **Transfer the keystore's allowlist doctrine.** The keystore decision in
  `2026-07-25-0946-keystore-entry-policy.md` classifies names that can never be
  registrations. A checkout's allowlist is its Git tree, while ward must also
  stage `.git`; the narrow OS denylist is an exception over a fail-closed
  default, and a missed artifact remains a hard failure.

## Refute-First Verification

An independent fresh-context lens found that the initial preflight buffered
unbounded porcelain output, collapsed metadata beneath ignored directories,
and could miss tracked metadata, assume-unchanged edits, or case collisions.
It also identified conditional `DT_UNKNOWN` misclassification and Unicode
bidirectional log spoofing.

All were confirmed and closed: NUL records are streamed under record and
entry caps; ignored leaf files are enumerated separately; tracked and abnormal
index entries are gated; status forces `core.ignorecase=false`; staging uses
`DirEntry.Info` before exclusion; and unsafe display code points are quoted.
The lens re-ran against that revision and reported no remaining actionable
findings. GitHub review then demonstrated that the preflight still ran before
repository-config canonicalization, allowing a configured clean filter to
execute on the host. The final ordering copies and canonicalizes first, then
diagnoses the immutable private snapshot before any VM launch. A widened
config-isolation refute pass found that status could still descend into an
initialized gitlink whose nested config was not canonicalized. The final
preflight rejects every non-blob index mode before status and tells status to
ignore submodules, so Git never enters a second repository context. A
subsequent GitHub pass then exposed `.git/commondir` as another route around
the canonical file. The direct filesystem gate now also rejects `commondir`
and `config.worktree` before Git runs. The root cause was twice assuming
`.git/config` was the only local authority Git could reach: first across
recursion, then within one repository layout. The closed enumeration is now:
system and global config are disabled, ambient Git environment is replaced,
includes are rejected, alternate common/worktree config is rejected, and
nested repositories are rejected by index mode without status descent. The
unchanged guest proof still fails closed across concurrent source mutations
while the snapshot is being copied.

Revisit when the seed source stops being a daemon-owned Git checkout, the
reference host adds another unavoidable metadata file class, or the guest
proof gains a safe authenticated diagnostic channel.
