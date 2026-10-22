# Ward Density on Apple Container

- Status: complete
- Tested: 2026-10-22
- Runtime: Apple container 1.1.0 (`5973b9c`), guest kernel `vmlinux-6.18.15-186`
- Host: Mac Studio, Apple M1 Max, 10 cores, 32 GiB, macOS 26.7.1
- Plan target: `docs/plan.md` §5.4 Machine Capacity, §5.7 ward class

## Conclusion

Once a ward has built, it costs the host its declared memory plus about 140
to 155 MiB of virtual-machine overhead, and it keeps that memory until the
container is deleted. Reserving the declared size alone undercounts every launch, so the
budget should reserve the declared size plus a fixed per-VM overhead. That
changes the reservation the plan states, so it is a gated plan edit, filed
as #2188 and not made here.

**Concurrent wards were not measured.** The steps for 4, 6 and 8 concurrent
writers need a host with no other containers. For more than an hour this
host ran another Freeside ward while swap stood at 17 of 18 GiB, so adding
up to 18 GiB of benchmark wards risked critical pressure under live work.
CPU overcommit and the slowdown point stay open in #2187.

**Upgrade verdict: upgrade to 1.6.0, as its own unit, for security fixes, not
density.** No release from 1.2.0 to 1.6.0 lifts a known limitation or changes
the ward class or its required capabilities. 1.6.0 does fix a pull-by-digest
verification flaw that the ward's image gate cannot detect on 1.1.0, and 1.3.1
and 1.4.1 fix content-store path traversal. Two changes break or alter
Freeside first (`--scheme auto` removal, a new guest kernel), so the upgrade
starts after those fixes and needs a fresh conformance proof.

**Size discrepancy.** The brief for this spike gave the verification default
as 4 CPUs and 1024 MiB. `daemon/internal/ward/sizes.go` gives verification 4
CPUs and 2048 MiB, the same as the writer; only review and conformance launch
at 1024 MiB. This spike measured the code's writer default and changed no
size.

## Measurements

The tables are the result; the raw samples and logs stay outside the
repository. Rerun the method below before comparing.

### Per-Ward Memory

| Measure | Result |
| --- | --- |
| Idle host footprint, project image, 2048 MiB declared | 270 MiB (peak 285) |
| Idle host footprint, project image, 1024 MiB declared | 241 to 256 MiB |
| Idle host footprint, exporter image, 1024 MiB declared | 239 to 255 MiB |
| Guest `MemTotal` at 2048 MiB declared | 2113 MiB |
| Host footprint during a build, 2048 MiB declared | 2.0 GiB within 30 s of start; peak 2186 to 2203 MiB |
| Guest cgroup `memory.peak` during the build | 1954 to 1962 MiB, page cache included |
| Memory-limit kills during the build | 0 |
| Host footprint after the build ends | 2185 to 2186 MiB |
| After `drop_caches` in the guest (guest free 1.2 to 2.0 GiB) | 2185 to 2186 MiB, unchanged |
| After also deleting the build cache in the guest | 2185 to 2186 MiB, unchanged |
| After `container delete` | VM process exits; the host's free and inactive pages recover |

An idle ward's whole VM costs 240 to 285 MiB, which varies little with the
declared size or the image. Once a build has touched the whole guest, the
VM holds the declared size plus 137 to 155 MiB for the rest of its life:
freed guest memory is not returned while the VM lives, as 1.1.0 documents.
The guest sees only 65 MiB of that extra; the rest is the VM process's own. A writer that has built once costs
its full declared size until it is deleted.

### Start and Teardown

| Measure | Result (3 runs each unless noted) |
| --- | --- |
| `run --detach` to first `exec` answering | 0.89 to 1.02 s; 2.08 s for the build step's launch (1 run) |
| Ward path: create | 0.05 to 0.07 s |
| Ward path: inspect | 0.02 s |
| Ward path: start | 0.85 to 1.07 s |
| Ward path: create to first `exec` | 1.01 to 1.22 s |
| `stop`, idle ward | 0.08 to 0.15 s; one outlier at 3.24 s |
| `delete`, idle ward | 0.13 to 0.39 s |
| `delete`, after a build | 1.89 s and 2.87 s (one per build run, 2 runs) |
| VM process gone after `delete` | Every `run --detach` run; not checked on the ward path |

Start and teardown are seconds, not minutes, so a VM per launch costs little
next to an agent run.

### Single-Ward Build

| Measure | Result |
| --- | --- |
| Workload | `go build ./... && go test ./... -count=1` in `daemon/`, cold build cache |
| Wall time | 392 s |
| Packages passing / failing | 47 / 6 |
| Host CPU idle during the build | 0 to 15% most of the time |

The 6 failing packages fail for environmental reasons, not memory: the
guest runs as root, so tests that inject a write failure through a read-only
directory see the write succeed, and one test needs `ps`, which the image
lacks. They still compile and run, so the workload is a fair density load.
An earlier run seeded only `daemon/`, failed 12 packages on missing
repository paths, and is not used for timing; its memory figures match the
table and are folded in.

### Concurrent Wards

Not measured; see the conclusion. The script's `concurrent N` step is ready
and refuses to start while other containers run. #2187 tracks running it on
a quiet host.

## Host Load Caveat

This host was not idle. Before and during every step it ran other
applications and, at times, other sessions' work: an Xcode build, a
`go test` of `freesided`, the prod and dev daemons, two registry containers
and the buildkit container, and from 16:24Z another session's 4-CPU,
6144 MiB container. At the start of the build step macOS reported memory
pressure 2 (warning), about 14 GiB of swap, about 15 GiB in the compressor,
and under 200 MiB of free pages. Pressure stayed at 2 throughout. Memory
figures per ward are read from the ward's own VM process and are not
affected; wall times and CPU idle are, and they are a floor on this
workstation, not a clean-host benchmark.

## Memory-Budget Recommendation

- **Reserve declared size plus a fixed per-VM overhead.** Count 256 MiB per
  container VM on top of its declared memory. The measured overhead at full
  use is 137 to 155 MiB above the declared size; 256 MiB leaves margin for
  other images and sizes, which this spike did not build. Because a launch's helpers run one after another inside the
  launch's reservation, one overhead per launch is enough.
- **Keep reserving the full declared size.** Freed guest memory is not
  returned, so a ward that has built once holds its declared size until
  delete. Reserving recorded peak instead would overcommit.
- **On this host, don't rely on half of RAM.** The default prod budget of
  16 GiB fits seven writers at 2048 + 256 MiB, not the eight that 16 GiB /
  2048 MiB suggests. This Studio is also a workstation already at warning
  pressure before any ward runs: about 14 GiB of swap and 15 GiB in the
  compressor with one ward or none. Until the concurrent steps are measured,
  the owner should set the Studio's budget explicitly rather than take the
  default, and start no higher than four writers' worth (about 9 GiB). That
  is a recommendation for the owner's setting, not a code default.
- **The plan's per-instance text needs a change.** §5.4 says the daemon
  "reserves its declared memory against the budget", and plan revision 94
  (#2169) carries the same rule to each execution host. Adding the overhead
  changes what #1598 implements, so it is a material plan edit. It is filed
  as #2188 and is not made in this change.

## Release Assessment: 1.2.0 Through 1.6.0

Assessed against the release notes of 1.2.0, 1.2.1, 1.2.2, 1.3.0, 1.3.1,
1.4.1 (no 1.4.0 was released), 1.5.0, and 1.6.0 (2026-10-20), the upstream
issues named below, and the 1.6.0 documentation, read on 2026-10-22. Nothing
was installed or run on a newer release.

### Known 1.1.0 Limitations

| Limitation | Newest release (1.6.0) | Evidence |
| --- | --- | --- |
| Freed guest memory is not returned to the host | **Unchanged** | 1.6.0's technical overview keeps the 1.1.0 text: pages freed inside the VM are not relinquished to macOS. No release mentions ballooning. |
| No hot detach of a volume | **Unchanged** | No release adds a detach or hot-unplug command. apple/container#1867 (dynamic resources) is open. |
| No workspace snapshot | **Unchanged for named volumes** | 1.2.0 adds `container export` for live containers; 1.6.0 adds `container commit`, which snapshots a running container's root filesystem into an image. Neither exports a named volume. |
| No exit status | **Unchanged** | apple/container#2402 and #1501 are open. |
| Image digests resolve only through a registry | **Unchanged**, and see the security list below | No release changes local digest resolution. |
| vmnet NAT breaks under a VPN | **Unchanged** | apple/container#1881 and its fix PR #1882 are open. 1.5.0 fixes egress loss after localhost DNS changes (#2256), a different failure. |

### Conformance Assertions

| Assertion (`daemon/internal/ward/conformance.go`) | Effect of 1.2.0 to 1.6.0 |
| --- | --- |
| Zero published ports and sockets | No change to the mechanism. 1.2.0 fixes a published-port forwarder buffer (CVE-2026-64773) that the ward never exposes. |
| Read-only remount | No change found. 1.2.0 adds OCI masked and read-only paths as runtime defaults; they don't touch the workspace mount, but they are new guest state the re-proof must observe. |
| Networkless export (`--network none`) | No change found. 1.6.0 reworks network attachments to add DNS aliases, so the no-network sentinel's inspect shape (`runtime_cli.go`) must be re-observed. |
| Credential volume detach | Still no host hot detach, so the class still needs VM destruction. |

**Nothing found changes the class or the required capabilities.**
`fresh_vm_read_only_volume_handoff` remains the strongest class the runtime
supports. `supports_credential_volume_detach` and
`supports_workspace_snapshot` stay false. Live export (1.2.0) and live
commit (1.6.0) are same-VM primitives, so they don't satisfy the separate-VM
rule.

### Changes That Break or Alter Freeside

- **Breaking: `--scheme auto` was removed in 1.3.0.** The `--registry` mode
  of the image build scripts and of the project-image builder
  (`daemon/internal/projectimage/apple.go`) push and pull with `--scheme
  auto`, which fails on 1.3.0 or later. The local-registry mode passes
  `http` and is unaffected. Explicit `https` works on 1.1.0 too, so the fix
  can land before the upgrade (#2176).
- **Guest kernel changed (1.3.0, Kata 3.32.0 debug build).** The
  memory-limit detector (`launch_record.go`) parses the guest's memcg OOM
  report and the `/container/<id>` cgroup path, pinned by 1.1.0 boot-log
  fixtures. The configuration digest's runtime identity is the CLI binary's
  hash, not the kernel, so a kernel change alone would not invalidate a
  proof (#2177).
- **Behavior change: commands run when the image entrypoint is empty
  (1.5.0).** Freeside's images carry no `ENTRYPOINT` by design, and check 2
  compares the inspected command with the daemon's. The realized command
  shape must be re-observed.
- **Guest VM defaults changed (1.2.1):** `vm.overcommit_memory` and
  `vm.max_map_count`. This can change when a build meets its memory limit,
  so the density numbers above don't carry over to 1.2.1 or later.
- **`container system status` output reshaped (1.4.1)** and **build moved
  to a CLI plugin (1.6.0).** Freeside parses neither, so no breakage is
  expected; the re-proof confirms the build path.

### Security Fixes Relevant to the Ward

- **GHSA-vgrw-36j7-r93w (fixed in 1.6.0 only, medium).** A pull by digest
  did not verify that the registry returned the pinned digest, and the
  local image keeps the pinned reference as its name. The ward's image gate
  (`sameImage` in `conformance.go`) compares that reference string, so on
  1.1.0 it cannot detect a substitution made at pull time. The build scripts
  and the project-image builder also compare the descriptor digest that
  `container image inspect` reports, which probably catches it; that is
  unverified (#2178).
- **CVE-2026-64786 (fixed in 1.2.0).** A bare-name image `Config.Env` entry
  inherited the launching process's environment, which here is the
  daemon's. Check 2 compares the realized environment exactly and
  Freeside's images carry no `ENV`, so this is mitigated but not proven by a
  probe.
- **Containerization fixes in 1.3.1 and 1.4.1:** unvalidated OCI descriptor
  digests enabling path traversal in the content store, a container ID that
  could delete files outside its bundle, symlink-following on OCI layout
  load, and an unvalidated `WWW-Authenticate` realm.

### Upgrade Path

The upgrade is its own unit (#2189) and runs only with the owner's approval
to install. It starts after #2176, because registry-mode image builds fail
on 1.6.0 without it, and after #2177, so the proof the upgrade records binds
the new kernel. It installs 1.6.0, reruns the full live conformance suite
and this benchmark, records the configuration digest change
(`ConfigurationBoundBackend` in `daemon/internal/exec/capability.go`; the
CLI hash changes, so the digest changes on its own), and updates the §5.7
text that names 1.1.0. An upgrade without a fresh proof is not allowed in
unattended mode.

## Method

Rerun this method unchanged so the next measurement is comparable. The
script is [`ward-density-bench.zsh`](ward-density-bench.zsh). It is a zsh
measurement record, not a repository tool, so it lives beside this doc
rather than under `scripts/`. The idle, cold-start and build steps ran one
revision earlier, whose count of other containers matched only `freeside-`
names and so missed UUID-named ones. The committed revision counts every
container except benchmark ones, buildkit and registries, and adds the
concurrent step's start refusal. Review then fixed three script bugs: the
failing-package count read each failure twice, the ward path never looked
up its VM process, so its teardown check was stale, and a start that never
answered hung the step. The tables use the corrected counts. Run each
step on a host with no other containers running, from any directory:

```sh
export IMG=<project image digest reference> HELPER_IMG=<exporter image reference>
export SEED=<dir holding a git archive of the repository> OUT=<log dir outside the repo>
zsh docs/spikes/ward-density-bench.zsh idle
zsh docs/spikes/ward-density-bench.zsh coldstart
zsh docs/spikes/ward-density-bench.zsh build
zsh docs/spikes/ward-density-bench.zsh concurrent 4   # then 6, then 8
zsh docs/spikes/ward-density-bench.zsh cleanup
```

- **Image.** The real writer and verification image for this repository:
  the project image `freeside-project-freeside-ai-freeside@sha256:15476780…`
  (agent-claude base, Go 1.26.10, golangci-lint 2.12.2, module cache warmed
  at commit `b8e8b08e`). The idle comparison adds the exporter image
  `freeside-exporter@sha256:e8aaf8b3…`.
- **Size.** The writer default from `daemon/internal/ward/sizes.go`: 4
  CPUs, 2048 MiB. Idle is also sampled at 1024 MiB. The benchmark never
  changes the sizes.
- **Shape.** One 4 GiB workspace volume per ward, seeded with a
  `git archive` of the whole repository at `2f76e54e`, because daemon tests
  read `prompts/`, `images/` and `scripts/`. No network (`--network none`).
  The main process sleeps and exits on SIGTERM, as a well-behaved agent
  would.
- **Workload.** In each ward, `cd /workspace/daemon && go build ./... &&
  go test ./... -count=1` from a cold build cache (the cache lives in the
  container's root filesystem, which is new per container), as a
  verification room runs it.
- **Host memory per ward.** macOS charges a container's guest memory to its
  `com.apple.Virtualization.VirtualMachine` XPC process, not to
  `container-runtime-linux`. The script finds that process through the
  container's open files and reads `phys_footprint` and
  `phys_footprint_peak` with `footprint(1)`. That is the number macOS
  counts against memory, compressed pages included.
- **Guest memory.** `memory.peak` of the container's cgroup inside the
  guest, read when the workload ends, and `/proc/meminfo` after it.
- **Host state.** Every 10 s or so: `kern.memorystatus_vm_pressure_level`
  (1 normal, 2 warning, 4 critical), free and inactive pages, compressor
  pages, swap used, CPU idle, free disk, and the count of other running
  containers (excluding buildkit and registries).
- **Wall time.** Measured inside the guest from the workload's start to its
  end. Concurrent wards start their workloads together.
- **Cold start.** `container run --detach` until the first
  `container exec` answers, three times; then the ward's own path (create,
  inspect, start, first exec), three times.
- **Teardown.** `container stop` then `container delete`, and a check that
  the VM process has exited.
- **Guards.** A concurrent step refuses to start while other containers run.
  While builds run, the step tears down and stops if pressure reaches
  critical or free disk falls below 20 GiB. The idle and cold-start steps
  and the concurrent step's launch phase are not guarded. A start that
  doesn't answer within 60 s fails its step.
