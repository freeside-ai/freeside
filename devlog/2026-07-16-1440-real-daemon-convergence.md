---
run: manual
stage: real-daemon-convergence
date: 2026-07-16
branch: feat/real-daemon-convergence
---

# Real-daemon convergence pass (issue #72, final slice)

Fulfills the Revisit-when of `2026-07-16-1030-saddle-cache-pairing.md`
(#67 merged, a listener composed). Mandatory note: the unit ships an
unauthenticated dev control surface beside the contract's trust
boundary, and it carries an owner scope decision that would otherwise
exist only in chat.

## Decisions

- **Scope amendment (owner decision, 2026-07-16).** #72's contract said
  "No daemon changes", but no composed listener existed anywhere
  (`NewHTTPHandler` was only ever constructed in Go tests, and
  `daemon/internal` is unimportable from outside the daemon module), so
  the convergence pass structurally required daemon-side composition.
  Owner chose one cross-component unit (this PR) over a separate
  signet listener unit, and a dev/test-only binary over starting
  `freesided` proper: plan §10 deliberately builds the product binary
  after interfaces survive real use, and this unit is that real use.
- **The harness is `daemon/cmd/freeside-signet-dev`: two loopback
  listeners, control strictly out of the contract mux.** The contract
  listener serves exactly `signet.NewHTTPHandler` under the real
  request authorizer; the choreography the mock exposed as actor hooks
  (mint code, rotate epoch, seed/advance items) lives on a second
  listener whose mux is built in `package main`, so production
  composition cannot inherit a control route. Both listeners refuse
  non-loopback binds (fail closed; the control surface is
  unauthenticated by design), and the pairing key is random per
  process, so minted codes die with the harness. Item seeding
  constructs the body server-side from `{id, item_version}` and runs
  the full domain and action-policy gates via `Service.PutItem`; the
  Swift suite never re-encodes domain shapes. Rejected: stdin commands
  (one-way, awkward mid-test from Swift), a unix socket (URLSession
  pain), a clock-control endpoint (see the expired-code decision), and
  `freesided` proper (above).
- **Convergence finding, fixed client-side: RFC 3339 is wider than the
  stock transcoder.** First live run: every date field in a real
  response failed to decode. Go marshals `time.Time` with fractional
  seconds and a zone offset; the generated client's `.iso8601`
  transcoder accepts only whole-second shapes (and
  `.iso8601WithFractionalSeconds` only fractional ones); the mock
  always emitted whole-second UTC, so the mock suites could never see
  it. Both shapes are legal `date-time` under the contract, so the fix
  is a lenient client decoder (`RFC3339DateTranscoder`: try
  fractional, fall back to whole; encode canonical whole-second UTC),
  shared by live and mock clients through the factory configuration.
  Rejected: normalizing timestamps daemon-side — the daemon's output is
  contract-legal, a robust parser is the client's job, and
  `daemon/internal/signet` is outside this unit's amended scope.
- **Test 16 over the wire has one observable branch, and the suite says
  so.** §5.14 lets a revoked device's verbatim retry return its
  recorded result; the daemon implements that replay inside the
  service, but `NewRequestAuthorizer` 401s every revoked request before
  a handler runs, so the reject branch is the only one reachable over
  HTTP. The convergence test asserts that branch (pending slot
  survives, no false "not recorded", no new side effect, verified
  through a second device); the replay branch stays pinned by signet's
  service-level Go tests and the client's rendering of it by the mock
  suite. The client was built correct under either (#114 note).
- **Expired-code coverage stays with signet's clock-injected Go
  tests.** Driving expiry through the harness would need a
  clock-control endpoint or a 10-minute wait, and the daemon rejects
  expired and consumed codes byte-identically by anti-probing design,
  so the client-visible behavior is fully exercised by the consumed
  and never-minted probes (which the suite asserts read identically).
- **One serialized suite, isolation by identity.** All eight tests
  share one daemon process; test 8's epoch rotation reads as a restore
  to anything in flight, so the suite is `.serialized` and each test
  pairs its own devices and seeds unique item IDs rather than
  resetting shared state. Test 8 also drops the mock test's
  revision-decrease assertion: the real revision is monotonic across
  epochs by design (`store.NewEpoch` never resets it); the epoch
  change itself is the invalidation.
- **In-memory stores behind live clients.** The pass targets
  client-daemon protocol convergence; Keychain and disk-cache custody
  are already unit-covered where they can fail honestly, and headless
  CI cannot host a login keychain.

Revisit when: `freesided` proper composes a production listener (plan
§10) — the harness should shrink to or be replaced by that composition
plus a dev flag, and the §5.2 loopback/Tailscale binding rule moves
there; and when #68 lands conversations — the convergence suite is the
natural home for the client halves of tests 5, 6, 7, 10, and 12
against the same harness.
