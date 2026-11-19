# The First Execution Host Is a Mac Mini

An owner decision of 2026-11-18, made in a session that compared Freeside's
execution-host plan with T3 Code's remote environments. It changes which
machine plan §5.7 names as the first increment, an owner choice that would
otherwise exist only in chat, so a mandatory note.

The plan edit is a wording change under Document Gating, with no revision
bump: the plan still commits to a first increment on an owner-held Mac of
the Studio's runtime and backend class, and only the machine it names
changes. The owner can overrule that classification in review.

The owner chose a dedicated Mac mini as the first execution host over the
M1 Max MacBook that `2026-10-22-1042-execution-hosts.md` named, because the
MacBook's proposed budget no longer holds the work it was sized for, and a
dedicated host removes the two conditions that budget was built around.

## Decision

- **The first increment runs on a Mac mini the owner plans to buy.** As the
  owner described it: M6, 24 GB, arm64, dedicated to Freeside, with 18 or
  20 GiB for ward containers. The machine is not bought yet, so the
  enrollment ceremony records what the host reports.
- **Nothing else about the increment changes.** It is an owner-held Mac on
  the Studio's runtime and backend class with `linux/arm64` images, so it
  needs no new backend, image, or suite, and the hold on control-plane
  credential injection (#2175) does not gate it. The unit issues on tracker
  #2161 (#2162 to #2168, and #3112) name no machine and stand as filed.
- **The start triggers still gate the increment.** The owner decided which
  host goes first, not when. Buying the machine is none of the three
  triggers; see the open decisions below.

## Why the MacBook's Budget Stopped Holding

The earlier note proposed 12 GiB on the MacBook, counted as six writers at
the 2048 MiB writer default before the per-VM overhead was reserved (five
once it is), leaving 20 GB for the owner's own use of the machine. On
2026-11-18 the owner set `execution.writer_memory_mib=6144` on the live
root after a 2 GiB writer thrashed on file cache (tracker #3418, planning
notes). A launch reserves its size plus the runtime's per-VM
overhead, 256 MiB for Apple `container` 1.1.0, so a writer now reserves
6400 MiB and 12 GiB holds one.

Raising the MacBook's budget competes with the owner's interactive use,
which is what the 12 GiB protected. A dedicated host has no such use, and
it does not sleep, so the unreachable-host rule (decision 11 of the earlier
note) covers a rare case on it rather than a daily one.

## Sizing

Writers per declared budget, counting the 256 MiB overhead. #3404's sweep
decides whether about 4 GiB is enough once Go builds fit the room.

| Declared budget | At 6144 MiB a writer | At 4096 MiB a writer |
| --- | --- | --- |
| 18 GiB | 2, with 5632 MiB left | 4, with 1024 MiB left |
| 19 GiB | 3, with 256 MiB left | 4, with 2048 MiB left |
| 20 GiB | 3, with 1280 MiB left | 4, with 3072 MiB left |

Three writers at today's size need 19200 MiB, which is 18.75 GiB. The
Studio's live 24 GiB budget holds three (tracker #3418), so a third writer
on the mini is the difference between doubling writer capacity and adding
two thirds.

A budget that three writers fill holds nothing else. The smallest other
launch, a review, reserves 1280 MiB, a verification reserves 2304 MiB, and
a ward proof pass on the daemon host reserves 2560 MiB (`daemon/README.md`,
Bound Container Memory). So at 19 GiB any of those placed on the mini waits
for a writer to end, and at 20 GiB only a review fits. How an execution
host reserves its own conformance suite is not built yet.

Decision 10 of the earlier note let a dedicated host take half its memory.
Half of 24 GB is 12 GiB, one writer, so the owner's 18 to 20 GiB is a
declared budget above that half, set at enrollment.

## Revises

`2026-10-22-1042-execution-hosts.md`, in what it says about the first host.
Three passages carry the decision; its other passages that name the MacBook
as the first host now describe the Mac mini. The note stays as it was.

- **Decision 13, "Order: MacBook, then control-plane injection once #2175
  lands, then Linux."** The first host becomes the Mac mini. The rest of the
  order stands.
- **Machine Facts and Sizing, the MacBook entry.** Its 12 GiB assumed a
  2048 MiB writer; the reasoning above replaces it for the first host.
- **Owner Decisions Needed, "Whether the MacBook's declared budget is
  12 GiB."** Replaced by the budget decision below. The other open
  decisions there stand.

## Owner Decisions Still Open

- **The declared budget: 18, 19, or 20 GiB.** 19 GiB buys the third writer
  and leaves macOS 5 GiB rather than 4, with no room beside three writers
  (Sizing). 18 GiB holds two writers and the launches that follow them. If
  the mini ever runs host-side macOS verification, that work needs memory
  too; whether the host verification executor reserves against the budget
  was not checked.
- **Whether a trigger must hold before the increment starts.** The
  capacity trigger reads recorded `machine_capacity` holds, which
  `freesided cause-counts` reports; nobody has read it for this purpose.
- **Placement order.** The plan prefers the daemon host, so the Studio's
  three writers fill before the mini takes one. The earlier note left
  local-first open. A per-host placement preference, recorded for #2166 in
  tracker #2161's planning notes of 2026-11-18, would let the owner prefer
  the mini without changing the default.
- **Whether the MacBook is enrolled later.** Not decided.

## Checks Before Enrollment

Owner-run prerequisites, recorded on tracker #2161 the way the Linux
desktop's Tailscale and OS refresh are. None is verified.

- **Restart with nobody logged in.** A headless host must bring its runner
  back after a reboot. FileVault blocks automatic login, and turning it off
  leaves the host's credential volumes unencrypted at rest. Whether the
  runner or Apple `container` needs a logged-in session is not established.
- **Runtime version.** A host whose runtime version has no recorded
  overhead is eligible for no launch (plan §5.4), so the mini runs a
  recorded version or the overhead is measured first.
- **Cores.** Each writer is capped at 4 CPUs. CPU is capped and not
  reserved, so a low core count slows three writers and blocks none.
- **Disk.** The plan budgets no disk. A writer's build trees and the
  project images need room that nobody has sized.

Revisit when the machine bought differs from the one described, when #3404
settles the writer size, or when host-side macOS verification is assigned
to an execution host.
