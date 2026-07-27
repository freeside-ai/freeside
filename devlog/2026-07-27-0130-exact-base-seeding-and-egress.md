# Exact-Base Workspace Seeding and Enforced Agent Egress

Work unit: #302. Mandatory note: contract-adjacent and returned-object
trust-boundary work, plus owner decisions that would otherwise exist only
in chat. Delivered as three stacked PRs under one issue; this note covers
the unit and is updated as the stack advances.

## Owner Decisions (2026-07-27)

- **Build real `provider_only` enforcement**, not a fail-closed refusal
  and not an operator waiver. Rejected: declaring the profile
  unenforceable and refusing it (honest, but blocks #237 on a follow-up);
  and permitting it under a recorded "unenforced egress" waiver on the
  1A.2 backup-waiver precedent (keeps #237 moving, but buys that with a
  second material plan amendment and a second standing exception). The
  mechanism is a per-run host-only network plus a daemon-side
  allowlisting CONNECT proxy, because `docs/history/decisions.md:20`
  already refutes in-guest enforcement.
- **Seed over `container cp`**, not a read-only bind mount for the
  seeder. Rejected: relaxing the structural bind-mount refusal, which is
  checks 1-2's isolation argument; and having the seeder fetch from the
  forge itself, which puts a live credential in a guest.
- **Three stacked PRs, not two.** The egress half is ~3000 lines across
  12 of 21 package files and introduces a runtime object class, a network
  server, and a capability-earning probe at once. Split at the seam where
  PR 2b adds no `Runtime` method and no `InspectReport` field.

## The Seeding Mechanism Is Forced by the Runtime, Not Chosen

The plan assumed the seeder could receive a copy without executing
("nothing from the image ever runs"). Probing Apple container 1.1.0
(commit `5973b9c`) refuted it and reshaped the design:

1. `container copy` against a created-but-never-started container fails
   with `invalidState: container ... is not running`. The seeder **must**
   execute, so a gate-authored payload now runs with the workspace
   read-write — a real trust delta, bounded by the payload being fixed,
   the image pinned and digest-checked, and the container credential- and
   network-free.
2. **A copy whose destination lies inside a mounted volume writes nothing
   and still exits 0.** Verified from inside the running container
   (`/dev/vdc /workspace ext4 rw`) and from a fresh container afterwards,
   for both a directory and a single file. This is the finding that
   matters most: the obvious implementation is silently broken, and it
   fails in the direction of running the writer against an empty
   workspace while reporting the declared base.
3. An in-guest `cp -a /seed/. /workspace/` does reach the volume. So the
   host stages into the seeder's **root filesystem** and the seeder's own
   command moves the tree onto the mount.
4. `container copy <src> <ctr>:/seed` with `/seed` absent creates `/seed`
   **as** `<src>`, not `/seed/<basename>`.
5. Copied files keep their mode and land owned by the host uid (501), not
   root.

Consequences encoded in the code rather than in comments: `copyArgs`
refuses a target inside the workspace mount, `Config.validate` requires
the stage and sentinel paths to be disjoint from it, and the sentinel is
a **second** copy because a directory copy is not atomic.

## Why the Observer Is a Separate Read-Only VM

Finding 2 means the copy's exit status is worth nothing, and the seeder's
own exit status is its account of itself. The attestation therefore comes
from a different container that did not write the workspace, mounts it
read-only (the same access class the exporter later uses), and emits its
proof into its own root filesystem for the host to export and parse,
bound to the invocation's unpredictable ownership token. It runs before
the writer because the base is a pre-writer fact.

Rejected: folding the readback into the seeder's script (the writer
vouching for its own write, through the read-write handle); and copying
`.git/HEAD` back to the host (needs a second runtime primitive and proves
what one file says rather than what a read-only mount presents).

Cost accepted: two extra VMs per seeded handoff.

## Refute-First Findings (PR 1)

- **Confirmed by execution:** the full sequence works on the reference
  runtime — seeder receives, copies, exits on its own; a read-only
  observer reads the expected `.git/HEAD`; a declared base the workspace
  does not hold is refused. Pinned as `TestLiveWorkspaceSeeding`, which
  also asserts the created-but-not-started copy refusal so a runtime
  upgrade that relaxes it surfaces the trust question instead of burying
  it.
- **Rejected by verification:** the plan's Mechanic A (seeder never
  starts) is impossible; the first end-to-end attempt failed because
  `copy` renames rather than nests, which is why the staged-tree assertion
  checks `/seed/.git` and not `/seed/<name>`.
- **Rejected by execution (fake):** 22 induced failures, including the
  copy that succeeds while seeding nothing, all fail closed under the
  check that names them, and every one also asserts the
  credential-bearing writer never started.
- **Accepted by decision:** the seeder executes with the workspace
  read-write (unavoidable, bounded as above); the observer's proof is
  produced by the pinned image, whose provenance is a trusted
  configuration input, the same boundary the networkless-export note drew
  for `SuiteFixture.AgentImage`.
- **Found and escalated, not fixed here:** `--internal` leaves the *host*
  reachable from the agent VM, so host services bound to that interface
  or `0.0.0.0` — including the daemon's API — are in reach (#326, a
  dependency of #237). And the egress proof stays ward-internal, so
  unattended admission never requires it (#327, a contract unit plus a
  §5.7 amendment).

### Fresh-Context Review Round (Pre-Push)

An independent refute-first pass found no blocking defect and five real
ones, all fixed:

- **Locale-dependent hex test in the observer.** A shell bracket range is
  collated, not byte-valued, so under a UTF-8 locale `[!0-9a-f]` does not
  reject `A`–`E`. Reproduced: `LC_ALL=en_US.UTF-8` attested forty
  uppercase `A`s as a detached commit. `verifyBaseProof` re-tests
  host-side, so nothing could have been accepted; the defect was that the
  guest was strict only by accident of the pinned image's environment
  while its comment claimed otherwise. Fixed by `LC_ALL=C` in the script.
- **Verified one path, copied another.** `verifySeedSource` resolved the
  source through symlinks; `seedWorkspace` staged the caller's unresolved
  path, leaving containment advisory against a symlink swap. Fixed by
  returning and staging the resolved path.
- **Guest wait raced the host's copies.** The seeder's give-up budget
  equalled one `SeedTimeout` while both host copies (bounded at
  `SeedTimeout` each) happen after it starts, so a large legitimate copy
  could trip a spurious refusal. Budget now exceeds the bounds it races.
- **Fake modeled around the runtime rather than after it.** It ignored
  `targetDir` and gated the silent discard on a manual flag, so a
  regression aiming the staged copy into the mount would have passed CI
  while seeding nothing. The discard is now derived from the container's
  mounts; verified by inducing exactly that regression and watching the
  ordering test fail with production's signature.
- **Fake's unseeded observer produced no proof**, so the "copy succeeded
  but seeded nothing" case exercised the missing-file branch instead of
  the `git_dir=absent` branch production takes.

### Codex Review Round 1 (P1: HEAD Is Not Content)

Confirmed and fixed. The observer proved HEAD matched the declared base
and stopped there, but HEAD is a pointer: a workspace can carry the right
one over a tree that is empty, partial, or altered. The intended producer
makes it concrete rather than theoretical, and the reviewer named it
exactly: `publish.Transport.FetchBase` moves HEAD to the base and never
checks anything out, so handing its directory to the gate seeded a
workspace with a `.git` and no files, which the old attestation passed.

The fix attests content within what ward can prove without git: the host
refuses a source with no working-tree content (so the FetchBase-shaped
case fails on the host, naming what is missing), and the observer reports
a digest over every file in the workspace that the host recomputes over
the source it verified. They agree only if the tree that landed is the
tree that was approved, which also closes partial and altered copies. The
digest covers paths and content, not modes or ownership, because
`container copy` does not preserve ownership across the host boundary.
Verified on the reference runtime that the Go and BusyBox implementations
agree.

**Deliberately not closed here:** whether the approved tree is *faithful
to the commit HEAD names*. That needs git in the observer image, and the
exporter image ships none; #302's scope routes an image change to the
images unit, so it is #330 rather than a widened ward unit. Stating the
boundary matters more than the gap: the gate now attests "the workspace
holds the tree the daemon staged and declared as base X", not "the
workspace is commit X".

### Codex Review Rounds 2 and 3

Round 2 (P2): the note's headings were sentence case where AGENTS.md
requires title case for new Markdown, and every recent note already
complies. Swept all seven headings rather than the two cited.

Round 3 (P2, and the more interesting one): the tree digest covered paths
and bytes only, so a workspace whose scripts lost the executable bit
attested as the approved tree. A git tree records that bit, so the digest
was not covering what it claimed. The stated rationale had conflated two
things: ownership genuinely is not preserved across the host boundary,
but **modes are** — which had been asserted without ever being tested.
Probed the runtime before acting: 0755, 0644, and 0600 all survive
`container copy`, `cp -a`, and the read-only remount unchanged, so
including the bit could not fail an honest seed. The digest now covers
per-file content against path plus the set of user-executable paths, and
the live test seeds an executable file so Go's `Perm()&0o100` and
BusyBox's `find -perm -u+x` are proven to agree on real content.

The lesson repeats the earlier rounds': an assertion about the runtime
that was never executed is not evidence, even when it sits in a rationale
comment.

Declined, with reasons: `$(cat)` drops NUL bytes, so a `.git/HEAD` of
NUL + 40 hex would attest that SHA — it needs a hostile seed source that
also matches the caller's declared base, which buys an attacker nothing
the declaration did not already give them. And `WorkspaceRef` has no
caller yet; it is the ward lane's published definition of
`exec.StartSpec.Workspace`'s opaque shape, consumed by #237, not
speculative surface.

Revisit when the reference runtime changes `container copy`'s
running-only requirement or its silent discard into a mount: both are
load-bearing, and the second is the reason the attestation exists in this
shape.
