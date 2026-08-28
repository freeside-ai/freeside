# Legacy Keychain Query Scope

Work unit: [PR #1000](https://github.com/freeside-ai/freeside/pull/1000),
which makes every legacy-backend query in `KeychainCredentialStore` carry an
explicit `kSecUseDataProtectionKeychain` value. This mandatory
credential-custody note records why the Data Protection Keychain decision in
`devlog/2026-08-26-1110-data-protection-keychain.md` stands and what actually
failed in the Wave 6 exit run.

## Decision

Kept the Data Protection Keychain as the authoritative macOS credential
backend, and fixed the legacy-cleanup query, over reverting macOS to the
file-based login Keychain (PR #997). The reversal rested on the observation
that a Data Protection item vanished about a second after a successful,
read-back-verified add. That observation was real but its cause was not the
Data Protection Keychain: the store's own `load()` deleted the item.

On macOS, `SecItemCategorizeQuery` treats a query that omits
`kSecUseDataProtectionKeychain` as targeting both keychains, and
`SecItemDelete` then runs against both. The store's "legacy" queries omitted
the key, so the post-read legacy cleanup in `load()` (Data Protection item
found, delete the legacy copy) removed the Data Protection item it had just
returned. Every later read was `errSecItemNotFound`, the per-request token
provider produced no token, and the device appeared revoked. Setting the key
to `false` on legacy queries scopes them to the file-based Keychain alone;
other platforms ignore the key.

Rejected the login-Keychain reversal because its premise (a non-sandboxed
app cannot rely on Data Protection items persisting) is false: Apple requires
profile-authorized entitlements, not App Sandbox, and the installed app has
them. Rejected removing only the post-read cleanup and leaving the queries
flag-less, because the migration and explicit-deletion paths issue the same
legacy queries, a flag-less read merges both keychains, and Apple's guidance
is to set the key on every operation.

## Refute-First Findings

Confirmed from Apple's Security source (`OSX/libsecurity_keychain/lib/
SecItem.cpp`, `SecItemCategorizeQuery`, `SecItemDelete`,
`SecItemMergeResults`): key absent targets both keychains; `true` targets the
Data Protection Keychain only; `false` targets the file-based Keychain only;
a flag-less copy prefers the Data Protection result.

Confirmed on the operator's Mac with a probe bundle signed under the
installed app's identity, entitlements, and provisioning profile, on a
throwaway service: a Data Protection item persisted across a delay and across
processes; a flag-less delete returned `errSecSuccess` and removed it; a
`false`-scoped delete returned `errSecItemNotFound` and left it intact.

Confirmed by the test fake: the previous fake keyed backends on whether the
key was `true`, so it modeled the flag-less delete as file-only and hid the
defect. The fake now mirrors the real routing, and the regression case
(repeated loads keep the authoritative item) fails against the unfixed store.

Accepted residual: the fix was proven at the SecItem API level under the
app's signature, not by re-pairing the installed app end to end.

## Revisit When

Apple changes the flag-less routing in `SecItemCategorizeQuery`, or the
macOS client becomes sandboxed and the file-based backend is retired.
