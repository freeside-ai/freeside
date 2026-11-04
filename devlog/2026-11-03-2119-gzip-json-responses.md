# Gzip Wraps The Whole Signet Mux

#2523 asked for compressed JSON so a full `/sync/bootstrap` download shrinks
(48,465 to 6,596 bytes on the `representative` fixture). #2532 later made
`daemon/internal/signet/http.go` a Tier A path, because it is where requests
meet the paired-device bearer check. The owner ruled on 2026-11-03 that this
unit meets the Tier A bar rather than landing under the earlier one, so this
note records its decision and its refute-first pass.

## Decisions

- **Chose to wrap the whole mux** in `compressJSON`, authenticated and
  unauthenticated routes alike, over wrapping only `/sync/bootstrap` or only
  authenticated routes. Every JSON route benefits, and the wrapper decides
  from the response's Content-Type, so the octet-stream attachment route
  stays untouched without a route list to keep in step.
- **Chose to decide once, when the status is written,** over buffering the
  body. Headers set after `WriteHeader` are dropped, and buffering would hold
  whole bootstrap snapshots in memory.

## Refute-First Pass

A fresh-context reviewer tried to prove the change wrong at `3dc863d8`, with
the signet suite, throwaway httptest tests over a real server, and a
benchmark.

- **Disproved: an auth outcome, status, or auth header changes.** The
  wrapper reads only `Accept-Encoding` and the response Content-Type, passes
  the status through, and touches only `Vary`, `Content-Encoding`, and
  `Content-Length`. A 401 kept `WWW-Authenticate: Bearer` and its body; the
  mux's text/plain 404 and 405 pass untouched.
- **Disproved: a compression side channel (BREACH).** The only secrets in a
  JSON response are the pairing grant's `device_token` and ntfy topic. Each
  is minted fresh per pairing and needs a single-use code, so no request can
  be repeated against the same secret. The bearer token travels only in a
  request header and is never echoed.
- **Disproved: a corrupted or mislabeled body.** Checked over a real server:
  gzipped GET, empty 200, 204, 304, HEAD, `Vary` on every JSON response,
  attachments, and a panic mid-response (the client gets EOF, never a
  valid-looking body).
- **Allowed: unauthenticated requests cost more.** With `Accept-Encoding:
  gzip`, a 401 costs about 143 µs and 821 KB allocated against 2.8 µs and
  6 KB plain, and a tiny body grows (a 401 is 51 bytes gzipped). Rejected a
  minimum-size buffer or a writer pool: the listener binds only to loopback
  or the tailnet, each cost needs its own request, and the factor is
  constant.
- **Allowed: an oversized body no longer closes the connection.**
  `http.MaxBytesReader` closes it through an unexported method the wrapper
  cannot pass on, so the server reads up to 256 KB more to keep the
  connection alive. The 413 is unchanged and nothing is smuggled.
- **Allowed: attachments lose `io.ReaderFrom`,** so copying one takes the
  ordinary path. A performance cost only.

Revisit when the daemon listens beyond loopback and the tailnet, or when a
response carries a long-lived secret beside client-supplied content.
