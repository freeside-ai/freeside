# Signet Registers Its Routes From One Table the Auth Test Reads

#3258 closes a test gap at the paired-device bearer gate (Tier A, client
authority). `TestHTTPOnlyHealthAndPairingAreUnauthenticated` checked a
hand-written list of 19 routes while `NewHTTPHandler` served 25 behind
`h.authenticated`, so a new route could ship without the wrapper and without
the test noticing. All 25 were wrapped; the gap was in the test.

## Decisions

- **One route table feeds both the mux and the test,** over keeping the
  hand-written list or reading routes back from the mux. Go's
  `http.ServeMux` can't list its patterns, so `httpHandler.routes()` is the
  only registration point: `NewHTTPHandler` mounts exactly its entries, and
  the test reads the same entries' patterns through a test-only export
  (`export_test.go`). A route added to the table is probed without anyone
  remembering to list it.
- **Each entry writes its wrapping out,** over an `authenticated bool`
  column that a loop turns into `h.authenticated`. With a flag, the test
  could only check the flag; with the wrapper at each entry, the test sends a
  request with no `Authorization` header to the handler that is served and
  requires 401 with `WWW-Authenticate: Bearer`. The unauthenticated routes
  (`GET /health`, `POST /pairing`, `POST /pairing/preview`) are an explicit
  allowlist in the test, and each entry must be in the table, so a stale
  allowlist fails.
- **A route registered outside the table escapes the test.** That gap is
  accepted, not closed: the table's doc comment says it is the only
  registration point, and review holds it there. Catching such a route would
  mean wrapping or replacing `ServeMux`, which the gap doesn't justify.

## Refute-First Evidence

- **Served behavior unchanged: confirmed by comparison.** The old and new
  `http.go` served the same generated set of 7,224 requests (every
  pattern's path with three wildcard fills, plus trailing-slash,
  extra-segment, upper-case, and unrouted variants; seven methods; no
  credential, a valid one, and a wrong one). Status, `WWW-Authenticate`,
  `Allow`, and `Content-Type` matched on every request, and bodies matched
  everywhere except `/sync/bootstrap` and `/sync/revision` with a valid
  credential, whose bodies also differ between two runs of the old code.
  A line-by-line read of the table against the old `mux.Handle` block
  shows the same 28 patterns, handlers, wrapping, and order.
- **The test catches an unwrapped route: confirmed by mutation.** Serving
  `GET /devices` through a plain `http.HandlerFunc` made its subtest fail
  with status 200 and no `WWW-Authenticate` header.
- **A probe could reach the wrong route: confirmed by the fresh-context
  reviewer, and fixed.** Filling every wildcard with `x` let an unwrapped
  future route such as `GET /runs/{$}` or `GET /runs/{rest...}` send its
  probe to the authenticated `GET /runs/{run_id}`, which answered 401, so the
  test passed while the new route stayed open. The test now builds a probe
  mux from the same patterns and requires each probe to reach its own
  pattern; `{$}` probes the bare trailing slash and `{name...}` probes two
  segments. No current route uses either form.
- **The zero `httpHandler` in the export is safe: checked.** Building the
  table only binds method values and the `authenticated` closures; no
  handler runs, so the export reads patterns without a service.

Revisit when signet registers routes anywhere but `routes()`, or a pattern
gains a host part or a wildcard constraint that the test's `x` placeholder
can't satisfy.
