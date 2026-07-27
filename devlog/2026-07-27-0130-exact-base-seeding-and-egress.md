# Exact-base workspace seeding and enforced agent egress

Work unit: #302. Mandatory note: contract-adjacent and returned-object
trust-boundary work, plus owner decisions that would otherwise exist only
in chat. Delivered as three stacked PRs under one issue; this note covers
the unit and is updated as the stack advances.

## Owner decisions (2026-07-27)

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

## The seeding mechanism is forced by the runtime, not chosen

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

## Why the observer is a separate read-only VM

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

## Refute-first findings (PR 1)

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

Revisit when the reference runtime changes `container copy`'s
running-only requirement or its silent discard into a mount: both are
load-bearing, and the second is the reason the attestation exists in this
shape.
