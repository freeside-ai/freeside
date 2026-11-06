# Cached Token Sources Store Tokens Behind Pointers (#2686)

## Decision

- **Chose pointer map entries as a second defence alongside `Format`.**
  The `tokens` map in `CachedTokenSource`, `FollowUpFilingTokenSource`,
  `IssueReadTokenSource`, and `OnboardingTokenSource` now holds
  `*InstallationToken`. A struct that keeps one of these sources in an
  unexported field, rendered under any verb a pointer does not support
  (`%s`, `%q`, `%t`, and others), reaches fmt's bad-verb path. That path
  prints the source's struct at depth 0 and cannot call its `Format`
  through the unexported field, so before this change the cache printed
  raw, token `Secret` included. The bad-verb path reprints the source
  pointer under `%v`, which prints any pointer below the top level as an
  address, so each map value now shows only its address. Each source keeps
  its `Format`, which still guards direct rendering.
- **Revised #2237's rejection of pointer hiding, within its stated
  scope.** `devlog/2026-11-05-2041-cached-token-source-format.md` rejected
  hiding the cache behind a pointer because it relies on fmt's depth rule
  instead of an explicit method a reader can find. That objection still
  rules out pointers as the sole mechanism, so `Format` stays. It does not
  rule them out as a backstop on the one path `Format` cannot reach, and a
  comment on each `tokens` field names the depth rule so a reader can find
  it.
- **Rejected a pointer to the whole map.** The map would print at depth 1
  as an address too, but every use becomes `(*s.tokens)[key]` for no
  protection beyond pointer entries.
- **Rejected `Format` methods on each holder type** (`publish.Transport`,
  the publish `forge`, `specify.Fetcher`, the `cmd/freesided` wiring). They
  cover only the holders written so far; fixing the four sources covers
  every holder, including future ones.
- **Kept `Token` returning a copy.** A cache hit returns `*tok`, so no
  caller holds a cached entry and nothing mutates one after it is stored.

## Refute-First Pass

Each claim below was checked against the change before commit, with a
throwaway probe run against both the old and the new `tokens.go`.

- **The sweep would pass without the fix: disproved.** With only the
  extended `TestNoCredentialValueRenders`, each of the four holder entries
  leaked the fixture token under `%s`, `%q`, and `%t` (12 failures); with
  pointer entries the test passes.
- **A holder that is nested in another struct, rendered through a
  pointer, in a slice, array, or map, or in an `any` field, or a source
  held by its concrete type, still leaks: disproved.** Before the fix each
  of these leaked under `%s`, `%q`, `%t`, `%c`, `%e`, and `%U` (102
  leaking renderings across two sources); after it, none leak under those
  verbs or `%v`, `%+v`, `%#v`, `%x`, `%X`, `%d`, `%o`, `%b`, `%p`, and
  `%T`. fmt's bad-verb path restarts at the pointer that failed the verb,
  wherever it sits, and reprints it under `%v`, so the entries below it
  print as addresses. A pointer to a holder held inside another struct
  never leaked: the restart begins at that outer pointer, which already
  prints the source as an address.
- **A source held by value, not through a pointer, still leaks:
  confirmed, accepted by decision.** The fresh-context reviewer showed that
  then the original bad verb reaches an entry pointer itself, and the
  restart prints that token raw. It cannot happen: each source holds a
  `sync.Mutex`, so the copy fails `go vet` (copylocks); every constructor
  returns a pointer; and a zero-value source has a nil map, so its `Token`
  panics on the first store.
- **A holder rendered through `log/slog` text or JSON handlers leaks:
  disproved.** Neither leaked before or after: the text handler uses `%+v`,
  and JSON skips unexported fields.
- **The cache keys now print on the holder path: accepted by decision.**
  The registration, installation, and repository IDs show in a holder
  rendering. They identify a grant; they are not credentials.

The sweep's verb list carries `%t` as its representative bad verb; the
probe showed `%c`, `%e`, and `%U` take the same path with the same result.

Out of scope, unchanged: any `Secret` stored directly in an unexported
field still renders raw under every verb, a limit of `Format`-based
redaction that #2237 recorded.

Revisit when the four sources' cache logic is merged into one type, or
when fmt changes how it prints pointers below depth 0.

Scope: `daemon/internal/publish`, `devlog/`.
