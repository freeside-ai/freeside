# Lineup Probe: Admit Its Names to the Rig Namespace

Work unit: #2308. Mandatory note: it widens what rig cleanup and stale
recovery may act on (a destructive path).

## Decision: Admit the Two Exact Probe Shapes

Chose adding exactly `freeside-lineup-probe-[0-9a-f]{12}` to the rig
container pattern and `freeside-lineup-probe-[0-9a-f]{12}-egress` to the
network pattern, following the preflight credential observer's precedent
(`2026-08-18-1430-preflight-credential-observer-rig-recovery.md`). The
probe already authorizes both names before creating either, so binding
them keeps the rig's invariant: every resource a run under the lease can
create is enumerable from the manifest. Running the probe without the
lease, or renaming its objects, were contract non-goals.

## Refute-First Pass

A fresh-context reviewer tried to prove the change wrong.

- **Regex admits only the two shapes.** CONFIRMED by extracting the live
  patterns and testing near-misses (trailing newline, leading text,
  `-egressx`, doubled slice, the name as a volume). No other alternative
  starts with `freeside-lineup`.
- **No foreign object uses the names.** CONFIRMED: only
  `ward/lineup_probe.go` builds them; the other `freeside-lineup-probe`
  strings are a record version and host temp-directory prefixes. The
  slice comes from a fresh 128-bit random label.
- **A leaked probe container is cleaned; a bound-but-absent name is
  harmless.** CONFIRMED: `DeleteContainers` removes recorded containers
  by name and skips absent ones.
- **A leaked probe network is cleaned.** REFUTED, ACCEPTED, DEFERRED to
  #2317. Rig cleanup and release only prove networks absent, and stale
  recovery deletes networks only through review journals, which the probe
  lacks. After a SIGKILL between network create and teardown, the rig
  stays blocked until a manual `container network delete`. This fails
  closed and needs a design choice in `cmd/freesided`, outside this unit.
- **The tests prove the claim.** CONFIRMED: both new tests fail on the
  old patterns with the exit run's exact refusal; `ward` tests importing
  `daemonlock` adds no cycle.

## Finding: the Release Failure Was Not Prelaunch's

The exit run's failed live prelaunch couldn't release because
`RuntimeCLIActive` saw a live `container` process. Prelaunch didn't
leave one: the failing probe started nothing, every ward CLI call runs
through `procbound.Run` (wait, then SIGKILL the call's group), and
`container ls` leaves no matching process. The check's pattern,
`^(.*/)?container([[:space:]]|$)`, is host-wide and matches any command
line containing `/container` plus whitespace, since `.*/` spans spaces.
A shell running a test fixture named `container` matches, and several
sessions were running daemon checks on the host. The exact process
wasn't captured. Follow-up: #2318.

## Revisit When

The probe gains a third runtime object or a journal, the rig patterns are
restructured, or #2318 changes what blocks release.
