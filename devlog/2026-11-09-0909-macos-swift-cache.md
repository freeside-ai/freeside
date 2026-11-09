# macOS Verification Takes Swift Dependencies From a Proved Offline Cache

Work unit: #2880. Mandatory note: Tier A (the cache decides what macOS
verification builds, and `slot-run` seeds it as root). The issue's plan set the
design; this note records it with what the implementation settled and where
it departs.

## Decision

- **One key over every `Package.resolved`.** The key is a SHA-256 over JSON of
  a version string and the sorted list of each `Package.resolved`'s
  workspace-relative path and SHA-256. JSON keeps the encoding unambiguous for
  any file name. A workspace with none needs no cache. A `Package.resolved`
  that is not a regular file is refused, since SwiftPM would read through a
  symlink to bytes the key does not cover. The key and populate's copies
  skip `.git` and `.build` directories by one rule; with two rules, a
  `Package.resolved` under `.build` gave the room a key populate could never
  produce.
- **A missing or incomplete entry fails before the slot is leased.** The room
  computes the key, then requires `<root>/swift-cache/<key>/` to be a real
  directory with a regular `record.json` naming the same key and format.
  Anything else is `ErrSwiftCacheMissing`, naming the key, the files, and the
  populate command. The command never runs and no helper is called.
- **Mirrors keep the repository's name under a digest directory.** The plan
  named a mirror by the SHA-256 of its URL, #2183's rule. On this host SwiftPM
  derives a mirrored package's identity from the mirror URL's last path
  component, so a digest-named mirror no longer matched the identity
  `Package.resolved` records and the offline resolve reported an out-of-date
  resolved file. The layout is `mirrors/<sha256>/<name>.git`: the digest
  directory keeps names unique, and `<name>` keeps the identity. Verified
  offline with both SwiftPM and xcodebuild.
- **Seeding goes through `slot-run prepare`, mirrors read in place.**
  `prepare` takes the key as an optional fourth argument and copies
  `seed/job/` into the job directory and `seed/home/.gitconfig` into the slot's
  home the way it copies the workspace: `tar` created as the daemon user and
  extracted as the slot, with named members so no archive entry for `.`
  resets a directory's mode. bsdtar replaces a symlink at a target and refuses
  to extract through a symlinked directory, so a workspace that plants a
  `.swiftpm` symlink cannot redirect the seed. The helper refuses an entry
  unless every directory it reads is a real directory the caller owns that is
  not group- or world-writable, and reports the key it seeded; the room
  refuses a prepare that reports another. The mirrors stay in the entry at
  0755/0644, so a job copies kilobytes, not the 230 MB of mirrors.
- **Populate proves the entry offline before writing the record.** It works
  on two scratch copies of the checkout (skipping `.git` and `.build`): one
  resolves online with SwiftPM's manifest sandbox on and the operator's git
  config off, then the mirrors are cloned and the seed written; the other
  resolves from the seed with `GIT_CONFIG_GLOBAL` at the seed's gitconfig and
  every HTTP(S) proxy variable pointed at a closed port (`127.0.0.1:9`), and
  every `Package.resolved` must stay byte-identical. `record.json` is written
  last through a temp file, fsync, and rename; any failure removes the entry.
  The proxy closure covers git and curl but not URLSession, which ignores
  the proxy variables (the fresh-context review showed a URLSession request
  succeed with `HTTPS_PROXY` closed). SwiftPM fetches only binary artifacts
  and registry archives that way, so populate refuses both instead: a
  `Package.resolved` pin of any kind but `remoteSourceControl`, and an online
  resolve that left anything under `artifacts/` or `registry/downloads/`.
  Running the proof under `sandbox-exec` instead was rejected: the manifest
  sandbox cannot nest inside it, and the proof would then run untrusted
  manifests with only a network denial around them.
- **Only https URLs, both spellings.** A dependency URL must be https, contain
  no space or control character, and end in a usable repository name. The
  gitconfig rewrites both the `.git` and bare spellings with `insteadOf`
  (submodules never read `mirrors.json`), allows the `file` protocol, and
  marks each mirror `safe.directory`, since the slot reads mirrors the daemon
  user owns.
- **The toolchain is checked per job, not per entry.** `Config.Toolchain`
  pins the Xcode build number and the `swiftlang-` token and enters the
  configuration digest (document v2). The room reads both from the host
  before every job and refuses a mismatch. Entries are keyed on
  `Package.resolved` alone and record the toolchain they were proved with,
  for the operator; after an Xcode update the operator re-earns conformance,
  and deleting an entry repopulates it.
- **xcodebuild resolves through git instead of skipping resolution.** The
  plan had xcodebuild recipes disable automatic resolution. The masked
  acceptance case needs a command that tries the network when the cache is
  gone, and with resolution disabled xcodebuild fails on missing packages
  without trying. The recipe instead keeps resolution on with
  `-onlyUsePackageVersionsFromResolvedFile`, `-disablePackageRepositoryCache`,
  and `-scmProvider system`, so Xcode fetches through git and the seeded
  gitconfig. Seeded, it resolves from `source-packages/` and the mirrors;
  masked, it fails with `Could not resolve host`.
- **Nested sandboxes are turned off inside the room's.** SwiftPM and Xcode
  sandbox manifests, plugins, and macros with `sandbox-exec`, which cannot
  nest. SwiftPM recipes pass `--disable-sandbox`; xcodebuild recipes pass
  `-IDEPackageSupportDisableManifestSandbox=YES`,
  `-IDEPackageSupportDisablePluginExecutionSandbox=YES`, and
  `OTHER_SWIFT_FLAGS=$(inherited) -disable-sandbox` (without `$(inherited)`
  swift-collections loses its experimental-feature flags). The room's
  profile still denies the network and confines writes. Populate leaves
  SwiftPM's manifest sandbox on, since it runs with network.
- **The executor install owns `swift-cache/`.** Owner decision on 2026-11-09,
  after the landing batch handed the PR back: #2941's install, which landed
  first, refused a root holding anything it does not write, so a hand-made
  cache made the whole executor read as foreign. `freesided macverify install`
  now creates `<root>/swift-cache` empty, owned by the operator at 0755, and
  `remove` deletes it, entries and all, with the root's other entries. It is
  the one tree directory root does not own: install hands a root-owned one
  (an interrupted create) to the operator, refuses one another user owns
  rather than give that user's entries to the operator, and never reads
  inside it. Rejected: install tolerating an operator-made `swift-cache` it
  never writes or removes. That keeps a hand step after install became the
  only setup path, leaves `check` unable to say who owns the cache, and makes
  every removal leave the root behind. Deleting the cache on removal loses
  nothing populate cannot rebuild, and removal already deletes the slots'
  jobs and homes whatever they hold.
- **#2183's scripts are not factored out.** They are busybox scripts run in
  Linux image build stages; this unit shares only the URL-to-digest rule by
  reimplementing it.

## Measurements

On the reference host (Xcode 27.2, build 27B52, `swiftlang-6.4.2.1.9`), a
populate for Freeside's `app/` took 1m43s and left 12 mirrors (230 MB) and a
110 MB `source-packages/` seed. A second run reported the entry already cached
without running a tool.

## Known Gaps

- **A package without a `Package.resolved` can still resolve from the
  mirrors.** The seeded gitconfig rewrites every mirrored URL globally, so a
  package the key does not cover can resolve to whatever version the mirrors
  hold. It cannot reach the network, and every `Package.resolved` it adds
  changes the key.
- **Binary targets and registry packages are not served.** Populate refuses
  them by name. Freeside's `app/` has neither.
- **`insteadOf` rewrites by prefix.** The rewrite for `https://h/a/b` also
  rewrites `https://h/a/bc`, and relative submodule URLs resolve against the
  `file://` mirror. In a job that runs as the sandboxed slot; in the proof it
  runs as the daemon user over mirrors the daemon user made. No path that
  pulls a repository the daemon user owns into `source-packages/` was found.
- **Entries are not keyed to the toolchain.** An entry proved under one Xcode
  is still served after the pin changes. Deleting the entry repopulates it.
- **Populate runs untrusted manifests online.** The checkout's
  `Package.swift` files run, under SwiftPM's manifest sandbox, as the daemon
  user with network. The sandbox allows reads, so a manifest could encode a
  readable file into a dependency URL that the online resolve then fetches.
  The issue contract accepts this: populate is an operator command run on a
  checkout the operator chose.
- **The room live suite was not run here.** The executor is not installed on
  the authoring host. The recipes were run instead under the shipped profile
  as the operator's uid, from the populated entry. SwiftPM ran with a fresh
  home. xcodebuild ran with the operator's home and the seed's gitconfig,
  because Xcode takes its home from the passwd entry, not `HOME`. Seeded,
  both passed; masked, both failed with `Could not resolve host`.
  `swiftcache_live_test.go` runs them as a slot.

## Refute-First Findings

A fresh-context reviewer tried to break the diff. It found no root or
cross-slot escalation.

- **Confirmed, fixed:** the URLSession gap in the offline proof (above), and
  the room and populate keying differently under `.git` or `.build`.
- **Confirmed, covered:** nothing tested the helper's seed refusals. The live
  suite now has `prepare` refuse a malformed key, an entry with no record, and
  a group-writable seed. Prepare needs root and slot accounts, so no unit test
  can run it.
- **Disproved by experiment:** bsdtar 3.5.3 refuses to extract through a
  symlinked intermediate directory ("Cannot extract through symlink") and
  replaces a symlinked final file without touching its target. Mirror names
  cannot traverse (`^[A-Za-z0-9_]` first, under a digest directory). git
  escapes the gitconfig values. `record.json` is renamed in last, so a failed
  populate never leaves a record, and `RemoveAll` on a symlinked entry
  removes only the link.
- **Allowed:** a lowercase `package.resolved` on case-insensitive APFS gets no
  key, so the job runs uncached and fails on the denied network rather than
  with `ErrSwiftCacheMissing`. It fails closed. The prefix rewrite, the
  toolchain-agnostic entries, and untrusted manifests online are recorded
  under Known Gaps.

A second fresh-context reviewer tried to break the install ownership of
`swift-cache/` after the hand-back. It found no destructive or privilege
defect.

- **Confirmed, fixed:** removal deleted the helper before the cache, so a
  cache the operator made undeletable (an immutable file) failed removal
  after the helper was gone, and a rerun no longer recognized the root.
  Removal now deletes the cache first. Tests now also cover a cache with the
  wrong mode and assert the unknown-operator finding exists.
- **Disproved:** a symlink at `<root>/swift-cache` reads as foreign (`Lstat`),
  and only root can create or swap that path; `RemoveAll` does not follow
  links inside; install never reads inside the cache and slot-run reads it
  as the caller, so the operator gains no power over root-run code.
- **Allowed:** removal deletes a cache whatever uid owns it, as it deletes
  jobs and homes. Only root can create an entry under the root.
