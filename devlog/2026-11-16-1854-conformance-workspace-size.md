# Ward Conformance Seeds a Run-Sized Workspace

Found by #2382's live run, the first to run a proof pass against a real
repository bigger than the fixture's.

## Finding

- **The proof pass seeded a real base into a 64 MB volume.**
  `runReservedConformance` built its `ward.SuiteFixture` without
  `WorkspaceSizeMB`, so the suite's synthetic default (64 MB) applied, but
  the fixture's seed is the project's real base checkout. A depth-1
  checkout of `freeside-ai/freeside` is about 105 MB on disk. Real runs
  get 4096 MB (`exec/claude/spec.go`).
- **The overflow reads as a categorical proof mismatch.** The seeder's
  `cp -a` fails partway. The gate ignores the seeder's exit status by
  design, so the observer reads a partial tree and its base proof fails:
  `observed_base_identity: base proof reports an unexpected value for a
  required key`. That message is deliberately categorical (naming the key
  would confirm a guessed nonce), so nothing pointed at the volume.
- **Nothing exercised it before.** The fixture repository is tiny, and the
  gate's seed budget (`MaxSeedBytes`, 512 MiB) never related to the
  volume size, so the gate staged seeds the volume couldn't hold.

## Decision

- **The proof pass gets a run's workspace size**
  (`claude.WorkspaceSizeMB`), so conformance proves the seed a run will
  actually receive.
- **A seeded handoff refuses, on the host and by name, a seed whose file
  bytes exceed its volume.** `seedWorkspace` caps the byte budget at the
  volume size, which turns the overflow into "seed source exceeds the N
  MiB workspace volume" before any container starts. File bytes are a lower
  bound on what the volume needs, so the refusal never rejects a seed that
  fits. A seed under the bound that still overflows (block and metadata
  overhead) is left to the observer, as before.
- **Rejected: estimating the on-disk need** (block rounding, inode and
  journal overhead) to refuse borderline seeds too. The estimate depends on
  the guest filesystem's parameters, which the host doesn't control, and a
  wrong estimate would refuse seeds that fit. The observer stays the
  authority on what landed.
- **Rejected: requiring every seeded volume to hold the whole seed budget**
  as a configuration invariant. It would reject every existing test and
  review fixture that pairs a small volume with the default budget, for no
  case a real run reaches.

## Refute-First Pass

A fresh-context reviewer tried to refute the change:

- **The cap refuses a seed that fits: disproved.** The budget counts the
  snapshot's bytes, which are exactly what the seeder copies, and no real
  path reaches the cap: runs and the proof pass use 4096 MB, reviews
  8192 MB, against a 512 MiB budget. `CreateVolume`'s `-s NM` can only make
  the volume smaller than the MiB cap, never the cap stricter.
- **4096 MB suite volumes break a budget: disproved.** Machine capacity
  counts memory, not disk, and the suite creates and reaps its volumes one
  after another.
- **A gate assertion or golden changes: disproved.** No golden pins a
  volume size.
- **Confirmed and fixed:** the canonical git-config growth refusal didn't
  name the volume when the volume set the cap; it now does.
- **Allowed by decision:** `WorkspaceSizeMB << 20` wraps only at an
  operator-set size of 2^43 MB or more, where volume creation fails first.
  The value is operator configuration, not untrusted input.
