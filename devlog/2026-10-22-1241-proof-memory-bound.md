# Proof Memory Bound

Issue #2139 sizes the project-image proof to the room it vouches for. The
proof ran its containers at Apple container's default of 4 CPUs and 1 GiB,
while every verification room runs at 4 CPUs and 2048 MiB or more, so
Freeside's own daemon recipe failed the proof (`go vet` OOM-killed) in a
size no room uses.

## Choices

Chose the verification room's class default, 4 CPUs and 2048 MiB, over the
project's policy size for every proof container. Policy may raise a room's
size and never lower it, so the default is the floor every room serves and a
recipe proven there fits any room. Following the policy size would make one
image's proof depend on a value the image does not record. The proof takes
the size from `ward.DefaultLaunchSize(LaunchVerification)` through the
ward's exported size renderer, so the two cannot drift on flag syntax.

Chose to bound unreclaimable memory over `memory.peak`, an owner decision in
the #2139 session. The issue's acceptance bounded `memory.peak` (page cache
included) at 1536 MiB. Measurement showed that cannot be met by any recipe
option: the container's root filesystem is ext4 on disk, so `GOCACHE` writes
become page cache, which grows to the room's limit and is reclaimed under
pressure. `memory.peak` was 1648 to 2043 MiB on every step of every option.
The margin is therefore on `anon + shmem + kernel` from `memory.stat`,
sampled every 0.2 s, at most 1536 MiB, with no `memory.events` OOM count.
Meeting the literal bound would need a larger verification default for
every room, which the data did not justify.

Chose `env GOFLAGS=-p=2 GOMEMLIMIT=768MiB` over the plain recipe and over
`GOFLAGS=-p=2` alone. Per-step peaks of unreclaimable memory, one container
per step, at `bee7979b` in a Go image built by this branch's builder:

| Step | Plain | `-p=2` | `-p=2`, `GOMEMLIMIT=768MiB` |
| --- | --- | --- | --- |
| build | 854 MiB | 821 MiB | 774 MiB |
| test | 1567 MiB | 1140 MiB | 1201 MiB |
| vet | 1548 MiB | 1146 MiB | 1191 MiB |
| lint | 2021 MiB, 1330 s | 1989 MiB, 1030 s | 1183 MiB, 158 s |

`-p=2` brings build, vet, and test under the margin but cannot reach
golangci-lint, which type-checks the module in one process; its heap filled
the room. `GOMEMLIMIT` caps the heap target of every Go process, so lint
drops to about 1.2 GiB. At its peak the plain lint left almost no room for
file cache, and its wall time shows the reclaim cost: rerun at a load
average near 18, it still took 407 s (1999 MiB), against 158 s for the
chosen option at a load near 175. A `GOMEMLIMIT` of
1024 MiB measured the same on lint (1201 MiB); 768 MiB is the issue's
suggested value and leaves the margin for a growing module. A larger room
and a smaller lint configuration were not needed. The `test` step fails in
every option on the same 17 tests, the root-sensitive and `ps`-dependent set
#2140 owns.

The full recipe in one container, as the proof runs it, measured 329 s for
build and test (where `check.sh` stops on the failing tests) and 172 s for
build, vet, and lint, with unreclaimable peaks of 1099 and 1189 MiB. With the
849 s image build, a rebuild needs roughly 22 minutes of the one-hour bound.
Wall times came from a host shared with other workloads, at load averages
from 7 to over 300.

Revisit when any step's unreclaimable peak passes 1536 MiB, when the ward's
verification default changes (the `apple_test.go` argv pins fail first),
when #2140 lands and the full recipe runs end to end in the proof, or when
the module's growth makes `GOMEMLIMIT` thrash the collector.
