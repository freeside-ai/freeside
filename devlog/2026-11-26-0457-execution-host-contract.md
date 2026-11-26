# Define the Execution Host Record

Issue #2162. A contract unit: Tier B for the record, Tier A by path for the
one route line in `daemon/internal/signet/http.go`.

## Decision

The daemon keeps a durable record of each execution host (plan §5.7
Execution Hosts): its id and unique name, the account its runs execute as,
how the daemon reaches it, the operator's override of its cordoned count,
and the conditions that close it. Migration 0150 seeds the daemon's own host,
so a one-host deployment is a record like any other and no consumer
special-cases it. `GET /execution-hosts` lists the hosts to a paired device,
and `GET /health` advertises the `execution_hosts` feature.

Planning made five choices, each open to the owner's veto. The
implementation kept all five.

## Choices and What They Rejected

- **The daemon's host has the fixed id and seeded name `daemon`,** over a
  generated id or the machine's hostname. A later migration (#3641's host
  on a launch) must name the host in a column default, and a migration can
  neither generate an id it can predict nor read the hostname. Renaming the
  seeded host is a non-goal of this unit.
- **The daemon's host records no run-account name,** over reading the
  account at startup and storing it. A migration can't read it, and the
  only consumer (#3629) reads `daemon_owner` alone. `Validate` refuses a
  name on the daemon's host, so a later unit that needs the name changes
  the contract instead of writing one quietly.
- **Conditions are rows of their own,** one per host and condition, over a
  column per condition or a JSON list on the host row. Seven units observe
  conditions (#3625, #2165, #2167, #3112, #3423, #3631, and #2166 if it
  stores the full count). Separate rows let each set or clear its own
  without rewriting the others, and a new member needs no migration.
  Setting a condition the host is already in keeps the earlier `since`, so
  the record shows when the condition began, not when it was last seen.
- **Host writes move the server revision,** over daemon-internal writes on
  `InternalTx`. A client reads the hosts, and `store/tx.go` requires a
  revision bump for any row a client read shows. Every `Write` moves the
  revision, whatever it changes, so an observer that polls compares its
  observation with a `Read` first and opens a `Write` only when they
  differ. Each set or clear, the count override's included, also reports
  whether it changed anything.
- **The API shows no reach and no count override,** over the whole record.
  A client needs neither, the pinned host key stays in the daemon, and the
  override is #2166's input. Either can be added later without breaking a
  client, since responses tolerate added fields.

## Smaller Choices Made in Implementation

- **`ClosesEveryLaunch` on the condition** carries the two groups the issue
  names, so placement (#2166) asks the condition instead of keeping a list.
- **The API names the run account and the observed condition as schemas**
  (`ExecutionHostRunAccount`, `ExecutionHostObservedCondition`) rather than
  inline objects, so the generated Swift types have stable names #2168 can
  use.
- **The schema keeps CHECKs only for shape:** non-empty text, a 0-or-1
  owner flag, an override of at least 1, and reach columns set together.
  The condition column has no CHECK list
  (`devlog/2026-11-07-1113-enum-columns-go-validation.md`); `Validate`
  guards it on write and decode.

## Review

The route line is on AGENTS.md's Tier A Paths, so the change took the
refute-first pass through one fresh-context reviewer. It found nothing
blocking and confirmed the route sits behind the paired-device check,
the response carries no reach or override, and the digests match the
spec. Its six non-blocking findings:

- **Confirmed, fixed:** the `hosts` tag said the daemon's host is "the
  first", but the list is ordered by id, so a host whose id sorts before
  `daemon` lists ahead of it. The tag now says the daemon's host is always
  among them.
- **Confirmed, fixed:** the count-override setter didn't report whether it
  changed anything, which acceptance 3's "each set or clear" covers.
- **Confirmed, fixed:** this note said the changed result alone avoids
  revision churn. It doesn't: the result exists only inside a `Write`,
  which has already moved the revision.
- **Declined, unreachable today:** `Validate` checks a host id only for
  being non-empty, and host names are unique only case-sensitively. No
  writer names a host until #3625, which owns how an operator names one
  and is the place to set both rules.
- **Declined, no failure:** the mock omits the daemon's account name where
  the daemon sends `null`; the client decodes both to nil.

## Revisit When

- A consumer needs the daemon's account name, or a way to rename a host.
- A client needs a host's reach or count override.
