# Delete the Post-Merge Race Jobs

The owner chose, on 2026-10-14, to delete the daemon's three post-merge
race-detector jobs and their `ci-race` incident observer (#2019). This
reverses `devlog/2026-08-08-1103-main-race-incident-policy.md`, which chose a
`go test -race` run after every push to `main` with automatic incident
reporting. The same decision is decision 7 of the parallel-work proposal
(`docs/proposals/parallel-work-design.md` §3.1 in free-skills, revised by
freeasinbird/free-skills#298), replacing its earlier plan to make the jobs
reliable and then require them.

## The Assumption That Changed

The 2026-08-08 note expected the post-merge run to catch regressions worth
its cost. In two months it caught none. Its eight `ci-race` issues (#628,
#633, #694, #716, #841, #908, #1222, #1824) record no data-race report.
Triage found:

- Per-package timeouts (#841, #908).
- A byte compare on a clock field (#716).
- The observer's own API failure (#694).
- One test that read a map while the goroutine it watched deleted the entry
  (#628, fixed in PR #632). This was a test bug, not a daemon race.

#1222 then collected 30 observer posts with no triage, and the owner closed
it as not planned. Incidents nobody triages detect nothing.

The cost was real. In the three latest pushes to `main` before deletion,
the race jobs held three runners for 16 to 32 minutes each, so a push took 26
to 33 minutes to finish, against 7 to 15 for the slowest job a pull request
runs.

## What Stays

The required Linux and macOS jobs run the same suites, uninstrumented, so
the deletion removes no test. `go test -race` stays a local command. The
SIGKILL fixture keeps its 30-second readiness deadline under race builds,
since it costs nothing outside them.

## Rejected Options

- **Make the jobs reliable and require them on pull requests.** The
  proposal's earlier plan, chaining #1829, #1877, and #1152. It would spend
  that work and the latency on every PR to keep a detector with no catches.
- **Keep the post-merge run.** The 2026-08-08 choice. Its incidents went
  untriaged, and every push to `main` paid for it.
- **A scheduled or manual race run.** The owner chose none.

Revisit when a data race turns up by hand, or the daemon gains concurrency
the standard suite doesn't exercise.
