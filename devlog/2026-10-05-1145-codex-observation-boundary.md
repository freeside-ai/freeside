# Separate Account Claims from Usage Collection

Work unit: #1758. Owner decision of 2026-10-05, after #866.

The owner chose local Codex ID-token claims for account facts and retained
the credential-mutation boundary for the network usage read. The earlier
no-refresh-attempt gate combined those two questions. The fresh synthetic
account baseline succeeds without a token-endpoint POST; near expiry still
produces blocked tokenless POSTs with an unchanged store. Those attempts
disprove the original unconditional hypothesis without showing a credential
mutation.

[ADR 0004](../docs/decisions/0004-read-codex-account-facts-locally.md) promotes
the revisited revision-37/77 decision and records the alternatives. Plan
revision 80 is the material change; neither #868 nor a usage collector ships
as part of it.

The source check used upstream tag `rust-v0.147.0`:
`codex-rs/app-server/src/request_processors/account_processor.rs`,
`get_account_rate_limits_response`, calls `AuthManager::auth()`;
`codex-rs/login/src/auth/manager.rs`, `should_refresh_proactively`, uses
`CHATGPT_ACCESS_TOKEN_REFRESH_WINDOW_MINUTES = 5`. The daemon currently
decodes the access token for expiry (`jwtExpiry`); reading ID-token account
claims remains #868's implementation work. Decoding claims is observation,
not a fresh authentication or signature-verification assertion.

The daemon's refresh path retains the old ID token when the provider omits a
replacement, while advancing the auth store's `last_refresh`. That timestamp
therefore cannot date account claims. Present them as claims from the stored
ID token, using its own issuance time when available and leaving unknown age
unknown. This preserves the owner's stale-claims label without overstating
what the latest credential refresh checked.

That path can also accept a replacement ID token without comparing its
account claim. The local projection must therefore apply the existing fixed
account-binding invariant before attributing decoded facts to the identity.
A missing, unverifiable, or mismatched binding leaves those facts unknown;
it cannot change admission, credentials, or enrollment.

The policy therefore requires a margin beyond five minutes covering the
bounded invocation and clock skew, plus an access-only snapshot and
`provider_only` egress. #1714 fixes and tests the exact margin. An attempt
that egress blocks is recorded without claiming collection succeeded; the
collector never renews credentials itself. Replan #868 only after this plan
revision merges, so its cited policy and work contract agree.

Revisit when the CLI's refresh window or call path changes, or measured usage
collection cannot fit the declared lifetime margin.
