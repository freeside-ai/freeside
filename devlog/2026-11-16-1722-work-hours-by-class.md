# Work Hours Divide a Run's Time as the Strips Do, in the Reader's Zone

Records the choices behind `GET /work-hours` (#3241), the read the work
status view's over-time tab (#3242) draws. The definitions live in the doc
comments of `domain.divideRunTime` and `domain.WorkHoursOf`; this note
records why they read as they do.

## Decisions

- **The classes and their order are #3237's.** Every instant of a run's
  elapsed time goes to the first that applies: agent working, waiting on
  you, held, other. The strips (#3237) draw the same four in the same order,
  so a day's bar and a run's strip never disagree. Elapsed and agent working
  time are the run clocks' own (`domain.RunClocksOf`, #3236): the division
  reads the run's bounds and active intervals from the same code, and a test
  holds the totals to the timeline's clocks. If the owner reorders the
  strips' classes, the same change applies here.
- **Where two holds overlap, the one first observed later wins.** The store
  ends a hold when the next cause begins, so stored spans touch rather than
  overlap; the rule only makes the division total.
- **The client names the time zone,** over UTC days or the daemon host's
  zone. The view is read on a phone that may sit in another zone than the
  daemon, and the study behind tracker #3231 found that long waits follow
  the owner's workday, so a day has to mean the reader's day. A day runs
  from the first instant of its calendar date in that zone to the first
  instant of the next, so it is 23 or 25 hours across a clock change. Where
  a clock change skips midnight, the day starts at the change: Go's
  `time.Date` can resolve a skipped midnight to the hour before it, on the
  previous date, which would hand that hour to the wrong day; where one
  repeats midnight, it picks the later one, so the day starts at the first.
  The daemon embeds the zone database (`time/tzdata`) as a fallback for a
  host without one; `time.LoadLocation` still reads the host's first, so a
  host's database version, and on macOS its case-insensitive file names,
  can decide whether an unusual name resolves. A name that resolves names a
  real zone, so the refusal rule wasn't tightened further.
- **The read reuses the timelines' per-run reads,** over reading milestones
  and holds straight from their tables. Whether a run is finished comes from
  its authenticated, settled conclusion; a cheaper read could disagree with
  the clocks a timeline serves. The cost is one authenticated pass over
  every stored run per request, about what `GET /runs` costs, with no
  rollup.
- **Tier A by path only.** The route is one line in
  `daemon/internal/signet/http.go`, a Tier A file, so the unit took the
  Tier A bar: refute-first, a fresh-context review, and this note. The read
  itself is advisory (Tier C): it grants nothing, writes nothing, and skips
  and counts a run whose record fails the integrity check rather than fail
  the whole response. Its query parameters grant nothing, so they are only
  checked where they enter.

## Refute-First Findings

A fresh-context reviewer tried to break the change before it was committed.

- **Confirmed and fixed:** a period reaching past year 9999 (or from year
  0) served a 200 with no body, because the day bounds couldn't be encoded;
  the period is now refused outside 0001-01-01 through 9999-12-30. The
  read's `as_of` was taken before the read transaction, so a milestone
  committed in between made the totals exceed what the timeline served; it
  is now read inside the transaction after the runs, as `GetRunTimeline`
  does. A repeated midnight (Amman until 2021) started the day an hour
  late. The mock refused zone names the daemon accepts (`Asia/Kolkata`,
  `Etc/UTC`, `US/Eastern`).
- **Disproved by a check:** path traversal through `time_zone` (Go refuses
  `..` and absolute names); the `runBounds` extraction changing
  `RunClocksOf`; classes or held reasons summing past their totals after
  rounding.
- **Allowed:** the mock and the daemon decode a raw `+` and a malformed
  escape differently; only a hand-built URL reaches that, never the
  generated client. Each request authenticates every stored run whatever
  the period, the cost recorded above.

The plan proposed splitting the unit into a daemon computation and a route
unit; the owner kept it whole.

Revisit when the strips' class order changes, when the daemon starts
recording verification starts (a fifth class), or when the read's cost on a
real database calls for a rollup.
