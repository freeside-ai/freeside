# Cached Token Sources Redact Through Per-Type Format (#2237)

## Decision

- **Chose a `Format` method on each caching source over a shared
  mechanism.** `CachedTokenSource`, `FollowUpFilingTokenSource`, and
  `OnboardingTokenSource` each gained a pointer-receiver `Format` that writes
  only the type name, copying `IssueReadTokenSource.Format` from #748. The
  leak is that fmt prints values reached through an unexported field raw,
  without calling `Secret`'s `Formatter`, so a default rendering of a
  populated source printed every cached installation token.
- **Rejected a `Format` on a shared cache field type.** fmt cannot call a
  method on a value it reaches through an unexported field, so that method
  would never run, which is the very bug being fixed.
- **Rejected an embedded shared cache type.** Embedding would promote one
  generic `Format` onto every source, so each would render under the shared
  type's name instead of its own.
- **Rejected hiding the cache behind a pointer field.** It works only
  because fmt prints a nested pointer as an address, an implicit property of
  fmt's depth rule rather than an explicit method a reader can find.
- **Kept the pointer receiver.** Each source holds a `sync.Mutex`, so a
  value copy fails `go vet` (copylocks), every constructor returns a pointer,
  and no daemon struct holds a source by value.

## Refute-First Pass

Each claim below was checked against the change before commit.

- **The sweep would pass without the fix: disproved.** With only the
  extended `TestNoCredentialValueRenders`, all three sources leaked the
  fixture token under `%v`, `%+v`, `%#v`, `%s`, `%q`, and `%x`; with the
  methods the test passes.
- **The sweep never checked `%x` renderings: confirmed, fixed.** The
  fresh-context reviewer found that `%x` hex-encodes the strings it reaches,
  so the plaintext needles never matched a `%x` leak. The sweep now also
  searches for the hex-encoded token.
- **A nil source, a slice or map of sources, a source in an exported field,
  `%T`, `%p`, `%d`, or `log/slog` text and JSON handlers leak: disproved** by
  a throwaway probe. Each renders the type name, an address, or `{}`.
- **A source held in another struct's unexported field leaks: confirmed,
  not fixed here.** A holder rendered with `%s` or `%q` (any verb a pointer
  does not support) reaches fmt's bad-verb path, which prints the pointed-to
  struct at depth 0 and cannot call `Format` through the unexported field,
  so the cache prints raw. `%v`, `%+v`, and `%#v` of the holder print only
  the address. The same holds for the existing `IssueReadTokenSource`, and
  any `Secret` in an unexported field leaks under every verb, so this is a
  limit of `Format`-based redaction, not of these methods. Holders such as
  `publish.Transport` and `specify.Fetcher` keep a `TokenSource` in an
  unexported field; no rendering of one is known. Follow-up: #2686.

Revisit when a holder of a credential-caching source is rendered through
fmt, or when the four sources' cache logic is merged into one type.

Scope: `daemon/internal/publish`, `devlog/`.
