# Authenticate Source-Issue Research to the Managed Repository Only (#748)

Work unit: #748. Scope: `daemon/internal/specify`, `daemon/internal/publish`,
`daemon/internal/engine` (the two research call sites), `daemon/cmd/freesided`,
`daemon/README.md`, `docs/production-walkthrough.md`, and `devlog/`. The
research fetcher now sends an installation token, so this is credential-leak
work and the note is mandatory.

## Problem

The research fetcher sent no credentials, and GitHub's issue API answers 404
for a private repository. Every entry point that reads the source issue
(label intake's task namer and research, `freesided submit -task <bare URL>`,
and the composer) failed on a private managed repository, and the failure
read as a bare `HTTP 404` after a clean writer exit (#2214).

## Decisions

**Scope the token by repository ID, and rewrite the owner/name form to the ID
endpoint, over authenticating any URL that names the project's owner/name.**
A rename changes owner/name, so a stale or reused name could route the
credential at another repository; the ID can't move. The submit and composer
writers see only the task text, so they request
`/repos/<owner>/<name>/issues/<n>`; the fetcher sends that request to
`/repositories/<id>/issues/<n>` with the same suffix and query, keeps the
writer's URL as the envelope's `url`, and records the ID URL as `final_url`.
`recover` compares only `url`, so retries still bind to the writer's request.
Any path with an empty, dot, or percent-encoded segment, a leading-zero ID,
or a non-positive issue number is out of scope and goes without credentials,
so no in-scope spelling can resolve elsewhere on the server.

**Mint a separate read-only grant (`issues: read`, `metadata: read`) over
reusing the publication or filing token.** The writer's requests are
untrusted, so the credential that answers them should be able to do nothing
but read issues. `IssueReadTokenSource` follows `FollowUpFilingTokenSource`:
it revalidates trust and the installation before each cache lookup, caches
by registration, installation, and repository ID, and records every mint.
The exact grant comparison stays strict: if GitHub ever answers with
`issues: write`, the mint fails as `ErrGrantMismatch` rather than handing a
write credential to research.

**Keep the redirect strip.** An authenticated request that redirects loses
its credential, even to the same repository.

**Repeat a refused request once without credentials.** A 401 or 403 to an
authenticated request means GitHub no longer honours the cached token, not
that the issue is private, so the fetch tries the public path before it
fails (Refute-First Findings).

**Fall back to an unauthenticated fetch when the token can't be had, over
failing the fetch.** An installation whose grant can't mint the read token
still researches public issues, as before this change. A private issue then
fails with its cause in the error and the item reason: no token source
composed, the mint error, a token for the wrong repository, or an
authenticated miss (issue absent or no read access). The error text is built
from the status, the requested URL, and the mint error only, never the token
or the response body.

**Record no authentication flag in the research artifact.** The envelope's
`url`, `final_url`, and `status` are unchanged in shape, so `FetchRequest`,
the research artifact, and the domain types stay as they were and the unit
stays out of `kind:contract`.

**Treat a run whose project isn't registered as the zero scope.** Runs aren't
foreign-keyed to projects, and some fixtures name an unregistered project.
The zero scope authenticates nothing, so research behaves as before; other
project read errors still fail the pass. Submission and label intake both
have the project row by the time research runs (the specification admission
binds it for submissions).

**Give `IssueReadTokenSource` a redacting `Format`.** `fmt` renders values
reached through an unexported field raw, bypassing `Secret`'s formatter, so a
default rendering of the source printed its cached token; the redaction sweep
caught it. The same gap in `CachedTokenSource` and `FollowUpFilingTokenSource`
predates this unit. Follow-up: #2237.

**Add `NewFetcherWithIssueTokens` beside `NewFetcher`, over adding a
parameter to `NewFetcher` as the issue's contract sketched.** Changing
`NewFetcher` would have edited `daemon/internal/integration` tests outside
the declared scope; `NewFetcher` now delegates with a nil source.

## Refute-First Findings

A fresh-context reviewer tried to refute five claims against the diff, with
a probe test through a real `http.Client` (deleted afterwards).

- **Disproved: the token reaches another host, repository, redirect, or an
  out-of-scope writer URL.** Encoded, dot, empty, and case-variant paths,
  a trailing-dot host, and userinfo all go without it; the rewrite takes
  the ID from the scope, never the URL; a 301 arrived with no
  Authorization.
- **Disproved: the token enters the envelope, an error, a log, or an item
  reason.** Mint errors carry IDs and scope names only, and response
  bodies are discarded.
- **Confirmed and fixed: a token GitHub refuses failed a fetch that would
  succeed without it.** A cached token stays in use until expiry, so a
  revoked token, a suspended installation the janitor hasn't reconciled, or
  an exhausted rate limit answered 401 or 403 and failed even a public
  issue, with a reason that blamed the issue. The fetcher now repeats such
  a request once without credentials and names the refusal when that also
  fails. It does not evict the cached token: that needs a wider
  `TokenSource` contract, and the cost is one extra request per fetch
  until expiry.
- **Confirmed and fixed (hardening): the query went verbatim into the
  authenticated request.** A query with a space reached the request line
  raw. Only a plain query (unreserved characters, separators, well-formed
  escapes) is now in scope; any other goes without credentials.
- **Allowed: the Referer of an allowlisted redirect carries the rewritten
  URL.** It holds no credential, and unauthenticated research already sent
  it.
- **Allowed: after a rename, the old owner/name still rewrites to the
  project's ID.** A URL naming a repository later created at the old name
  then returns the project's issue under that name. No credential or other
  repository's data moves, and the project row is the daemon's binding.
- **Unverified: whether `/repositories/<id>/…` serves every issue
  sub-resource `/repos/<owner>/<name>/…` does.** Only a live request
  settles it; the exit run (#1613) covers the issue itself.

## Revisit When

- The first live private-repository run shows GitHub returning
  `issues: write` for a requested `issues: read`: request `write` on the read
  source and record why, rather than loosening the grant comparison.
- The specifier prompt package starts requesting the ID endpoint directly;
  the owner/name rewrite then only serves older packages.
