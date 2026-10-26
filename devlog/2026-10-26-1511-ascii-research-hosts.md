# Refuse Non-ASCII Research Hosts (#2340)

Work unit: #2340. Scope: `daemon/internal/specify` and `devlog/`. The
research fetcher is a credential-leak surface (`managedRepositoryURL` uses
`canonicalOrigin` to decide which URLs carry the installation token), so
the refute-first findings are recorded here. The gap was found by the #2330
refute-first reviewer (`devlog/2026-10-26-1108-github-page-issue-reads.md`).

## Problem

`canonicalOrigin` lowercased the host with `strings.ToLower`, which maps
`İ` (U+0130) to `i` and the Kelvin sign `K` (U+212A) to `k`. So
`https://gİthub.com/` passed an allowlist naming `https://github.com`,
while `net/http` converts a non-ASCII host with IDNA and dials
`xn--github-qyd.com`, a domain the policy never admitted. Any research URL
a specifier writes could reach this. No credential could follow: the token
only goes to a URL rebuilt from constants on the real `api.github.com`.

## Decisions

**Refuse any non-ASCII host in `canonicalOrigin`, over comparing origins
after IDNA conversion.** `net/http` dials an ASCII host exactly as written
(`idnaASCII` in `net/http/request.go` returns it unchanged), and DNS
ignores ASCII case, so for an ASCII host the lowercased origin compared is
the domain dialed. IDNA comparison would need `golang.org/x/net/idna`, a
dependency `daemon/go.mod` doesn't declare, only to admit internationalized
hosts the allowlist doesn't support. One check in `canonicalOrigin` covers
every decision: allowlist entries, the request URL, redirect targets, the
final URL on fetch and on recovery, and the token-scope check.

**Keep a non-ASCII allowlist entry an ordinary error, over
`ErrResearchURLRefused`.** The allowlist is operator policy, not untrusted
request data, and every other malformed entry already fails this way at
fetch time.

## Refute-First Findings

A fresh-context reviewer tried to refute the change against the diff, with
throwaway programs on go1.26.10, including a local TLS server recording the
dial address, SNI, and `Host` header over 38 host spellings.

- **Disproved: an ASCII host that passes dials a different domain.** The
  transport dials `idnaASCII(url.Hostname())`, which is the same
  `Hostname()` the check reads; every accepted spelling dialed and sent
  SNI equal to it, ignoring case. `github.com.` sends SNI without the dot
  but its origin `github.com.:443` isn't allowlisted.
- **Disproved: a percent-encoded or odd spelling slips through.**
  `url.Parse` rejects percent-encoded ASCII in a host (`%67ithub.com`,
  `github%2Ecom`, `%00`, `%2F`), as well as tabs, backslashes, bracketed
  names, and malformed ports. `g%C4%B0thub.com` (either hex case) decodes to
  `gİthub.com` and is refused, as are the fullwidth dot, long s, and Kelvin
  sign. Opaque `https:github.com` and `https:///github.com` have no host
  and are refused. Re-parsing `url.String()` never changed the host.
- **Disproved: a request path skips `canonicalOrigin`.** The request URL,
  each redirect (checked on the URL object the client then sends, after
  headers are copied, so the `Authorization` delete holds), the final URL,
  and recovery all go through `validateResearchURL`. The proxy is disabled,
  so no CONNECT target bypasses it.
- **Confirmed: the tests prove the fix.** Against the pre-fix
  `fetcher.go`, the three new URL cases, the redirect test (which followed
  the lookalike target), and the allowlist test all fail.
- **Disproved: uppercase ASCII regresses.** `GITHUB.COM` and
  `GitHub.Com:443` still pass.
- **Allowed by decision: recovery refuses an old non-ASCII final URL.** A
  research record made before the fix whose final URL was a non-ASCII host
  now fails recovery with `ErrResearchURLRefused`. Such a record is exactly
  the lookalike egress this unit closes, so failing closed is intended.
- **Allowed by decision: a zoned IPv6 literal passes the allowlist's "DNS
  host" check.** `net.ParseIP` returns nil for `fe80::1%github.com`, so an
  entry like `https://[fe80::1%25github.com]` parses as DNS-hosted. Only the
  operator writes the allowlist, the dialer's `publicResearchIP` still
  refuses non-public addresses, and allowlist validation is a non-goal here.

Revisit when a Go upgrade changes how `net/http` converts ASCII hosts, or
when the allowlist needs internationalized hosts.
