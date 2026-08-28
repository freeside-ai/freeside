# macOS Login Keychain Custody

Work unit: [PR #997](https://github.com/freeside-ai/freeside/pull/997). This
mandatory note supersedes the macOS custody decision in
`devlog/2026-08-26-1110-data-protection-keychain.md`; iOS keeps that note's
Data Protection Keychain decision.

## Decision

Chose the file-based login Keychain as the authoritative macOS credential
backend over the Data Protection Keychain. The earlier decision assumed that
a private provisioned access group made Data Protection items durable for the
installed, non-sandboxed app. The Wave 6 exit run falsified that assumption:
`SecItemAdd` returned success, but the item disappeared within about a second,
and subsequent authenticated requests were sent without a token. The login
Keychain retained the same credential and supported the complete authenticated
run.

macOS reads only the login Keychain and performs no migration or fallback. A
save best-effort clears a stale Data Protection copy before replacing and
verifying the authoritative item. Failure to clear that non-authoritative copy
does not block the real save because reads never consult it. Explicit identity
deletion still attempts both backends and reports the first real error, so a
failed cleanup cannot be presented as a successful revocation. iOS remains
Data Protection-authoritative with after-first-unlock accessibility.

Rejected keeping Data Protection authority on macOS because an acknowledged
write is not durable in the supported runtime. Rejected fallback between
backends because a stale credential must not shadow or resurrect a replaced
identity. Rejected migration on load because it would reintroduce the failed
backend and make an ordinary read destructive. Keeping the login Keychain item
under its default app ACL also preserves the existing prompt-free update model
when the installer retains the signing identity and bundle identifier.

## Refute-First Findings

Confirmed by the installed-app run: the supported macOS shape loses a Data
Protection item after an acknowledged write, while the login Keychain item
survives the update and authenticates later requests without a prompt.

Disproved by the credential-store cases: a stale or corrupt Data Protection
copy cannot shadow a macOS read; failure to clear that non-authoritative copy
cannot block a verified login-Keychain save; and a corrupt item or Security
error on the authoritative read fails closed instead of falling back. Explicit
deletion attempts both backends even after one fails and preserves the first
error, so partial cleanup remains visible and retryable.

Accepted residual: a best-effort clear can leave the stale Data Protection
copy behind. It carries no read authority, later saves retry its removal, and
explicit identity deletion continues to treat cleanup failure as an error.

## Revisit When

The macOS client becomes sandboxed, Apple documents and demonstrates durable
Data Protection Keychain behavior for the supported installed-app shape, or
the login Keychain ACL no longer recognizes same-identity updates reliably.
