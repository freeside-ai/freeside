# 0004: Read Codex Account Facts Locally

- Status: Accepted
- Date: 2026-10-05
- Decider: Ben Nelson-Weiss
- History: [revision 37](../history/decisions.md#revision-37) and
  [revision 77](../history/decisions.md#revision-77-subscription-usage-visibility-before-the-1b1-exit),
  revised by revision 80 (#1758, `docs/plan.md` §13 while current).
- Source notes: `devlog/2026-08-21-0405-provider-profiles.md`,
  `devlog/2026-10-02-1819-subscription-usage-visibility.md`, and the #866
  measurements beginning with
  `devlog/2026-10-05-1011-codex-account-probe-spike.md`.

## Context

Revision 37 gated the doctor account probe on proving that the Codex
app-server never refreshes outside the mutation lease. Revision 77 reused
that gate for usage observation. The account and usage paths have different
needs: account facts are already in the stored ID token, while usage requires
a provider response. Revisiting that shared gate promotes the decision here.

The #866 corrected synthetic follow-up in [PR #1759](https://github.com/freeside-ai/freeside/pull/1759)
with Codex 0.147.0 returned account facts both with an hour of token life and
with two minutes. The fresh case sent no `POST /oauth/token`; the near-expiry
case recorded four blocked refresh requests. The corrected
[control capture](https://github.com/freeside-ai/freeside/blob/5f62a73392f7b80b77cb4db8b0a7e10f77e17e49/daemon/internal/ward/testdata/codex_account_observed_control.jsonl)
records seven blocked refresh requests and zero transport failures. This is a
new measurement; the original PR #1753 record of six blocked requests and one
unexplained TLS failure remains historical evidence. All follow-up snapshots
stayed unchanged, and none contained a refresh token. The original
unconditional no-attempt claim is false; this is not evidence of a credential
mutation.

## Decision

Read Codex account facts by decoding ID-token claims in the daemon, without
starting the app-server. Present them as claims "from the stored ID token",
using its own issuance time when available; unknown age stays unknown. Never
use the auth store's `last_refresh` for claim freshness, because credential
refresh can retain an older ID token. Keep missing or malformed claims
unknown. Attribute account facts only when the decoded account ID matches
the identity or generation's fixed account binding; a missing, unverifiable,
or mismatched binding leaves the facts unknown without altering admission,
credentials, or enrollment. They cannot prove current revocation, current
plan, or current provider availability. Access-token expiry remains separate
from ID-token expiry. These are advisory facts, never admission or selection
authority.

For network usage observation, the rule is no refresh that could change a
credential outside the mutation lease. Preserve the access-only, read-only
snapshot and `provider_only` egress. A tokenless attempt blocked by that
egress is tolerated, recorded, and scheduled around; it supplies no successful
usage evidence. Credential renewal remains a separate lease-held operation.

In the pinned upstream implementation, the usage read calls `AuthManager::auth()`
and can proactively refresh within five minutes of access-token expiry.
Only collect with enough lifetime beyond that threshold to cover the bounded
invocation and clock skew. An unknown or insufficient lifetime defers
collection, preserving prior readings and timestamps, without affecting
execution admission. #1714 must measure both fresh and near-expiry cases
and set the exact margin and deadline before a collector ships.

## Alternatives

- **Keep app-server account reads behind a no-attempt gate:** rejected because
  launching it adds refresh behavior to a question answered by local claims.
- **Require a real account for #866:** rejected because its account answer
  comes from claims. Synthetic lifetimes isolate the refresh behavior without
  exposing a credential or claiming current provider validation.
- **Treat a blocked tokenless attempt as credential mutation:** rejected
  because no refresh-capable credential is present and the request cannot
  reach the provider. Scheduling around it avoids a predictable failed read.
- **Rely only on the lifetime check:** rejected because startup timing,
  revocation responses, or future CLI behavior can still attempt refresh.
  The snapshot and egress remain the safety backstops.

## Consequences

#866 finishes as a negative result and provides the harness for #1714. #868
needs replanning after this policy revision merges; its account facts no
longer require an app-server launch. The usage spike still needs its own
responses and update-notification evidence. No production implementation is
part of this policy revision.

Revisit when the pinned CLI changes its refresh behavior, a supported local
account schema changes, or a proposed collector cannot stay within its
declared lifetime margin.
