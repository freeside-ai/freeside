# Carve freesided's Startup in Place Before Moving It

Chose to carve `freesided`'s `run()` into named startup phases inside
package main (#2762) as the first of six links, over moving the daemon's
composition into `daemon/internal/` in one unit or moving blocks out of
`run()` straight into a package. The chain and its seams are below; the
planner files links 2 to 6 as their own issues.

## Why Carve First

- **The closure is most of the package.** A type-checked reference
  closure from `run()` at `28b858b1` reached about 16,600 declaration
  lines in 68 of `cmd/freesided`'s 86 files. About 5,000 of them come
  through the control-socket routes on `pairingControl`. One unit could
  not move that, and a package can't import main, so each link has to
  leave main compiling on its own.
- **`run()`'s blocks shared state through captured locals.** The
  attention service's options captured five values composed after the
  service exists. A block that assigned one couldn't leave `run()`
  without taking those closures with it. `startupBindings` gathers them
  behind a pointer, and each phase now takes its inputs as parameters, so
  a later link moves whole functions instead of untangling a closure.
- **Cleanup order has to stay in `run()`.** A `defer` moved into a phase
  runs when the phase returns, not when startup fails. Every
  failure-cleanup defer, the success defer between `startServing` and the
  loop starts, and the scenario listener's defer on `run()`'s named error
  stay in `run()`. The startup session closer is assigned through a
  pointer at its old point, so a Claude step that fails after the driver
  exists still awaits its credential-bearing sessions.

## How the Carve Was Done

- **Move-only, apart from two lines.** The ten blocks moved verbatim
  apart from indentation. Each phase that writes several values returns
  them in a small struct (`claudeWorkflow`, `claudeLoops`), so the moved
  bodies keep their `return nil, err` statements. Only the session-closer
  assignment and the mutex passed to image upkeep changed inside a moved
  body.
- **The planned blocks left `run()` at 415 lines,** over the 400 the
  issue set. The flag checks and the `-seed-fixture` pass moved too, in
  an authored commit, because each returned `run()`'s `(nil, err)` pair.
  `run()` ended at 385 lines.
- **No input structs.** Several phases take 9 to 12 parameters. Grouping
  them would either rewrite the moved bodies or add a prologue that
  unpacks the same list. The explicit lists are what links 3 to 6 turn
  into package APIs.

## The Chain

| Link | Change | Starts after | Tier |
| --- | --- | --- | --- |
| 1 (#2762) | Carve `run()` into named phases in package main | none | B |
| 2 | One engine-option builder that the Claude workflow and `composeScenario` both fill, with the scenario's omissions as explicit zero values and their reasons | #2762 | B |
| 3 | Move the process shell (the `daemon` type, `Wait` and `Close`, supervision, the listeners, readiness, run ID) to an `internal/` package, holding `pairingControl` behind an interface the package declares | #2762 | B |
| 4 | Move storage, attention, fake, and scenario composition to an `internal/` composition package | link 2 | B |
| 5 | Move the Claude composition and its background loops | #2552, link 4 | A |
| 6 | `run()` becomes config translation plus one call; `config` and its test seams become exported options | links 3 and 5 | B, or A if it touches a Tier A file |

- **#2552 gates link 5.** `composeClaudeWorkflow` calls
  `composeClaudeDriver`, which constructs `storeAdmissionAuthority`, and
  #2552 moves that authority out of `claude_driver.go`.
- **The control routes stay in main.** The shell takes route registration
  as an injected function, so `pairing_*.go` (Tier A) stays untouched.
  Moving the routes is outside this chain.

Revisit when a new composition mode, cleanup defer, or late-bound value
lands in `run()` before link 3, or when #2552 or another unit moves
`composeClaudeDriver`, `composeScenario`, or the `daemon` type first.
