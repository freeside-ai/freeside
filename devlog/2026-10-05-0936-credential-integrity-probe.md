# Credential-Integrity Probe

Issue #1630 is the probe half of the doctor credential-integrity check (plan
§10, §11 row 8). #1624 added the mark and the admission refusal
(`2026-10-03-1444-credential-integrity-mark.md`). This unit observes stored
credentials on the doctor schedule, records the mark, files the item, and
reports the finding.

A mark refuses new admissions on its generation and clears only by
re-enrollment, which #1639 has not shipped. Most decisions below follow from
that: a false mark has no way back yet, so the probe is built to miss damage
before it reports damage that is not there.

## Posture

- **The item is `advisory`, not `blocking`.** #1624 left the posture to this
  unit. The mark already closes admission for the one generation it names,
  and other enrollments keep admitting. A blocking `system_health` item
  impairs unattended admission for the whole daemon, which would widen one
  damaged store into a full stop. The item's diagnostic is
  `credential_integrity`, impairing `agent_credential`, the shape the Codex
  re-enrollment marker uses.
- **The finding stays out of `converge`.** `converge` files a blocking item
  for every unhealthy doctor code. The credential-integrity finding is
  appended to the report after it, and its items converge separately, one
  per marked generation, under an id prefix `converge` never matches.
- **The report is unhealthy while a current generation is marked,** so
  `freesided doctor` exits nonzero. Handling the advisory item does not clear
  the finding; re-enrollment does.

## Decisions

1. **Two digest conventions count as intact.** `auth add` records the
   SHA-256 of the token bytes. `auth adopt` records the observer's tree
   digest, which also covers inode, mode, link count, owner, and group. The
   generation does not say which convention wrote it, so the probe observes
   both digests and marks corruption only when the recorded digest matches
   neither. Rejected: recording the convention on the generation, a contract
   change this unit may not make.
2. **Truncation is a length check only.** A setup token shorter than
   `MinSetupTokenBytes`, the bound enrollment enforces on input, or an
   `auth.json` that is empty or ends before its first JSON value closes.
   Bytes that are not JSON, or that follow a closed document, are not
   truncation. Rejected: any content heuristic (token prefix, JSON field
   presence), because a wrong guess is a false mark.
3. **The corruption check keys on `RefreshStrategy`, not the client.** Only
   an `external` store is expected to keep the bytes its generation recorded.
   A store the daemon refreshes in place moves past its digest on the first
   refresh, so it gets no corruption check and the finding says so.
4. **Only a finished observation marks.** After observing, the probe opens
   one transaction that requires its read hold to still be live and the
   current generation to be the one it observed, and records marks inside it.
   That transaction runs even when there is nothing to mark, so "checked"
   always means "observed under a hold that outlasted the observation". A
   store it cannot finish observing is "not checked" with a fixed reason
   code. The cause goes to the daemon log only.
5. **The integrity observer extends the opaque gate script; it does not
   replace or parameterize it.** The handoff gate's command is unchanged
   byte for byte, and its proof parser still refuses the two new keys. The
   setup-token manifest policy is not used: it fails the whole proof on an
   empty token or a changed mode, which is the damage the probe exists to
   describe. Rejected: a new `CredentialManifestPolicy` member, which would
   reach every switch over that enum for one caller.
6. **Neither token fact can fail toward a finding.** A byte count the
   image cannot produce writes `unknown`, and a hash it cannot produce writes
   an empty digest. The parser refuses both, so the observation errors and
   the store is not checked. A token that is absent, a symlink, or not a
   regular file reports absent.
7. **The probe lists volumes before it mounts one.** The runtime creates a
   named volume on first mount. A probe that promises to write nothing must
   not create an empty volume for an enrollment whose store is gone.
8. **The Codex read uses the review lifecycle's reader and root.** The same
   bounded private-file reader, under `-review-input-root`, with the path
   required to resolve to itself, as the refresh requires. A stricter check
   would leave stores the daemon itself uses unchecked. A Codex store outside
   that root is not checked.
9. **One read hold per identity, a fresh holder per pass.** The observation
   context ends with the hold window, so no read outlives its hold. A crash
   leaves a two-minute hold that delays that identity's next refresh or
   enrollment.
10. **Item convergence plans in a read and applies in a write.** A store
    write always advances the client-visible revision, and most passes
    change nothing. A pass with something to change recomputes the plan
    inside its write transaction, so a scheduled pass and an on-demand pass
    cannot both file one generation's item. The item id is a function of the
    enrollment and ordinal; an item in any status blocks a refile.
11. **Startup and offline passes observe nothing.** The daemon's startup
    pass and a `freesided doctor` with no daemon report recorded marks and
    say so. Only scheduled passes and a doctor command served by a running
    production daemon start observer containers.
12. **The live probe runs outside the doctor convergence lock.** The
    unattended engine takes that lock before every dispatch, and a pass
    lasts as long as its observer containers. The scheduled pass observes
    first, then takes the lock and hands the doctor that one result. Marks
    need no lock: each is recorded in its own guarded transaction.
13. **A corruption finding marks only when a second observation
    reproduces it.** The tree digest's passes discard tool errors by design,
    so a tool or fork failure inside the observer moves the digest while the
    proof still parses. An `auth adopt` generation matches on the tree digest
    alone, so one incomplete observation would have marked it. A corruption
    finding that does not reproduce is dropped alone and reported as not
    reproduced. The length verdict is validated on its own in each
    observation, so a truncation both report still marks; two observations
    that disagree on it are "not checked" (`observation_unstable`). The
    second observation runs only on a suspected corruption, under the same
    hold.
    Rejected: an error-checked tree digest in the integrity script, which
    would be a second implementation of the gate's digest that must stay
    byte-identical to it. This does not cover a tool that fails the same way
    every time; see the image risk under Not Done.

## Not Done

- **No Codex corruption check.** A Codex refresh rewrites `auth.json` and
  appends no generation, so there is no digest to compare against.
  Follow-up: #1748.
- **No live check against an operator's adopted volume, and no defense
  against an exporter-image change.** The tree digest hashes `ls -ldi`
  output: inode, mode, link count, and the owner and group as the image
  renders them. Adoption records it under `auth adopt -exporter-image`; the
  probe reads it under the daemon's `-exporter-image`. The observer ran in
  the reference runtime against a throwaway volume, where both digests are
  equal with one image. An image that renders owners differently, or a
  volume recreated from a backup, reproduces a different digest on every
  observation and marks every adopted generation. The generation does not
  record the image it was digested with, so the probe cannot tell. This is
  the issue's corruption criterion as written; it is flagged to the owner in
  the PR.
- **No change to startup's treatment of a marked generation** (#1740). This
  unit is the first to record marks in a running daemon, so that owner
  decision now has production reach.

## Refute-First Findings

A fresh-context reviewer and the author's own checks tried to prove the
change wrong. Each claim is recorded with its outcome.

**Confirmed and fixed**

- **An incomplete tree pass marked an adopted generation.** Reproduced by
  the reviewer with an `ls` that exits 1: the proof parsed and the tree
  digest moved. Fixed by decision 13.
- **The scheduled pass held the convergence lock across the observer
  containers,** stalling unattended dispatch. Fixed by decision 12.
- **A replayed probe result would have outlived its pass.** The first
  version of decision 12 reassigned the captured probe, so every later pass
  would have reported the first pass's result. Fixed with a per-pass replay
  and a test.
- **A closed JSON document followed by other bytes read as truncated.**
  Found while moving the Codex check onto the strict decoder. Trailing data
  is now not truncation, with a test case.

- **An unreproduced corruption finding discarded a real truncation.**
  Raised by the Codex review on the first version of decision 13, which
  skipped the whole store. Only the corruption finding is dropped now.

**Disproved by a check**

- **The handoff gate changed.** With the integrity flag off, the command,
  parse order, unknown-key refusal, and return values are unchanged; the
  gate's parser test refuses both new keys.
- **A token the script cannot read reports short.** Run under `sh` and
  `dash`: an unreadable token writes an empty digest and `unknown`, which
  the parser refuses. Absent, directory, symlink, and FIFO report absent.
- **The observer does not behave this way in the real image.** The opt-in
  live test ran the script in the reference runtime on a throwaway volume:
  the tree digest equals adoption's, the token facts are right for an
  intact, short, and empty token, and a missing volume is not created.
- **A read can outlive its hold.** The store refuses a stale acquisition,
  the marking transaction rechecks the hold and the ordinal, and the
  observation context ends with the hold window (synctest case).
- **The advisory item blocks unattended admission.** The item's id prefix
  is distinct from `converge`'s, and the unattended gate skips advisory
  items.
- **A credential value reaches an output.** The command test plants a
  sentinel in two stores and finds it in no response, error, item, mark,
  log line, or database file. Ward errors are categorical and carry no
  proof content.

**Allowed by an explicit decision**

- **The tree digest is not stable across exporter images or a recreated
  volume.** The issue's corruption criterion compares against it. See Not
  Done; flagged to the owner.
- **A store read error can quote a raw account binding.** The text comes
  from `domain.ValidateEnrollmentIdentityBinding`, on a row whose binding
  disagrees with its parent. The write path refuses that row, so it takes a
  damaged database to reach, and every other reader of an enrollment already
  returns the same error. The fix belongs in `daemon/internal/domain`, which
  this unit may not change.
- **The item's only action is `acknowledge`, which does not close it.** It
  stays open until the generation moves, as the issue specifies. The
  no-refile rule covers an item closed by any other route.
- **Two overlapping passes can advance the revision once with no change.**
  The second writer recomputes an empty plan inside its write. The cost is
  one client refetch; the alternative is aborting a write on purpose.
- **A mutation-lease acquire fails while the probe holds its read hold.**
  That is what a shared read hold is, and the issue requires it. A Codex
  hold lasts one file read. A review launch that lands inside it fails the
  same operational check another reader's hold already causes.
- **The script's unit tests run the host shell, not the image's.** The live
  test covers the image. It is opt-in like the package's other live suites,
  which CI does not run.

## Revisit When

- #1639 ships `auth re-enroll`. A false mark then has a remedy, and the
  item's reason should name the command.
- #1748 makes a refresh append a generation. The corruption check then
  extends to Codex stores and decision 3 changes.
- The exporter image's `ls`, `find`, or `sha256sum` output changes. The tree
  digest hashes that output, so adopted generations would stop matching.
- An identity is skipped for a live mutation lease on every pass. The
  finding's detail shows it each time; a busy identity is never checked.
