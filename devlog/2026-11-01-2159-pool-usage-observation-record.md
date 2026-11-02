# The Pool Usage Record Derives Exhaustion And Trusts Only Its Collector

#1715 adds the usage observation record of plan §5.4, with the exhausted mark
and model-to-window map of revisions 99 and 103. These choices outlast the
unit, because the router (#2393), the quota park (#2421), and collection
(#1716) all build on them.

## Exhausted State Is Derived

Chose a pure predicate over readings and refusal marks instead of a stored
exhausted flag. A flag would need a writer at every clearing event, and two of
those events are the passing of time: a reset that nothing observes. The
derived form clears at the reset with no writer and no job, and it can't
disagree with the facts beneath it.

Refusal marks stay stored, because a refusal is a fact no reading repeats. A
mark without a reset stays until an allowed reading observed strictly after
it. A reading at the same instant says nothing about the order, so it doesn't
clear the mark.

When a refusal and an exhausted reading are both active on one window, the
later one supplies the mark's source and reset, and the refusal wins a tie.
That rule is the plan comment's assumption; #2393 and #2421 consume it, so a
change to it needs its own note.

## Readings Come From The Collector Alone

Chose to refuse a reading from any other enrollment at validation, and to
refuse a window on a record whose collector never answered or last answered
unsupported. Two enrollments on one subscription observe one allowance, so a
second source could only add a disagreement to resolve. Failover stays
impossible by construction (plan §5.4, no silent fallback).

## The Generation Gate Applies To New Readings

The plan comment refused any reading whose generation differs from the
collector's current one. Taken literally, that blocks every rewrite of a record
holding a reading from a replaced generation: a refusal mark, which writes
through the same accessor, and the failed refresh and partial update #1716
requires. A quota refusal would then fail to record exactly when a credential
had just rotated.

Chose to gate only the readings the stored record doesn't already hold. A
carried reading keeps naming its own generation, which the acceptance also
asks for, and a new one must be current. The same distinction closes a gap
validation can't see: a failed attempt may keep readings but can't add one, so
a first attempt that failed can't invent a window.

## Clearing And Ordering Are Decided At The Write

Clearing was first derived only from a window's current reading. An independent
review showed the gap: an allowed reading cleared a mark, an exhausted reading
replaced it, and the mark returned with no end once that reading's reset
passed. Chose to drop a cleared mark at the write over storing a cleared-at
instant, because a cleared mark says nothing more and the predicate then reads
only live facts. The same review found a window's mark and reading could each
regress. A window keeps the mark with the later `set_at`, and a new reading
must be observed after the one it replaces, whatever instant the write carries.

`RecordWindowRefusalMark` leaves the whole-record revision gate, which the plan
comment had it pass through. Its read and write share one transaction, so the
gate protected nothing and could only lose a provider refusal to a clock tie.

## Characterization Retires The Derived Pool's Record

The read gate fails closed on a record whose enrollment left its pool, and one
such record fails every listing. Characterizing an identity into a shared pool
leaves exactly that behind on its derived pool, with no enrollment able to
repair it. Chose to delete that record in `settleIdentityUsagePool`, the one
step that moves an identity. `UsagePool` is set once, so the pool left is always
the derived pool of that identity alone. Nothing of value goes with it: the
next collection reads the shared pool, and a live mark returns with the next
provider refusal.

## Refute-First Findings

The delete is a destructive path, so an independent reviewer tried to prove it
wrong, with throwaway tests in a temporary copy.

- **Disproved, a failed identity write loses the record:** the delete shares
  the identity write's transaction, and a forced failure after it restored the
  record.
- **Disproved, the record returns and wedges the listing:** after the delete, a
  mark and the default collector report `ErrNotFound`, and a whole-record write
  is refused by the enrollment gate.
- **Allowed, a live mark goes with the record:** the next run on the shared pool
  may be refused and parks until reset, which is the behavior for any unmarked
  window.
- **Allowed, a pool named after another identity's derived id:** nothing
  reserves the `identity:` prefix, so an identity declaring `identity:a` as its
  pool loses that pool's record when identity `a` is characterized. It takes an
  operator choosing that name, no production code sets `UsagePool`, and what is
  lost can be collected again.
- **Allowed, a same-instant replay after a dropped mark:** a late mark that an
  allowed reading already cleared leaves `[]` where the replay holds null, so
  the replay gets `ErrStaleWrite`. No data is lost.
- **Allowed, a collection stamped at or before a mark's instant:** it is
  refused and succeeds on the next cycle.

Two findings of the first review are declined by decision. A whole-record write
replaces the marks it omits, so a collector reads and writes in one transaction,
as the accessor's comment says. A refusal on a window with no map entry counts
for every model: a window of unknown scope is general by the contract, and
holding a model back is the safe direction. A pool with no enrollment can't
take a mark, because the record needs a collector; #2421 needs an enrolled pool.

## No Fixture-Only Mark Source

Chose two sources, `provider_refusal` and `usage_reading`, over a third for
fixtures. The record stays honest about what set a mark, so #2393's exit proof
seeds a real exhausted reading under an enrolled collector at its current
generation.

## Billing Renewal And Reset Credit Stay Out

The plan reserves both, and no supported credential exposes either. They are
added on evidence, as their own fields, never folded into a window's reset
(`devlog/2026-10-02-1819-subscription-usage-visibility.md`).

Revisit when a provider's window can't be named by one stable token, or when
#2389's verdict adds a field a reading lacks.
