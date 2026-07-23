# Owner Resolution Mint Gate

Issue: #247. Contract dependencies: #256 and #259.

## Decision

Chose repository `owner/name` as the public mint coordinate, but not as the
GitHub grant identity. The minter first re-validates the repository's existing
current automation trust profile, takes the owner-approved canonical repository
ID from that profile, then enumerates `GET /app/installations` under every
locally trusted registration and selects the unique installation account
matching the repository owner. The token request uses `repository_ids`, and the
response must return that exact ID plus the expected name. Rejected
caller-supplied installation IDs, bare repository names, and name-scoped token
requests because each can route around an immutable trust binding.

Chose numeric App ID as registration identity and validate every returned
installation's App ID, installation ID, target ID, account ID, and account
login before selection. A private registration may report at most one
installation, and that installation must match the registration owner's login
and numeric ID. A public registration may report several accounts; matching is
by the requested repository owner, and matches under more than one
registration fail as ambiguous. Errors retain only the locally trusted
registration ID, expected owner, and a closed reason, never returned account
text.

Chose the current trust profile as the onboarded-set source over a second
repository allowlist. This is the pre-token half of the existing publication
gate: it proves the repository is locally trusted before discovery or minting.
The candidate authorization and fresh workflow-drift decision still run after
the token-backed live audit, because requiring that later observation before
minting would be circular.

Chose cache identity `(registration App ID, installation ID, repository ID)`
and re-run both the trust gate and live installation resolution before every
cache hit. Rejected caching resolution for the token lifetime because an
uninstall, registration removal, repository re-approval, or owner-binding
change must stop even a still-unexpired credential from being handed out again.

Mint audit records carry the numeric App ID through `MintRecord` and, through
#256, the production SQLite row. Rejected a publish-only field after
verification showed `StoreRecorder` would silently project it away.

## Refute-First Verification

**Confirmed and fixed:**

- The original package-only audit change lost registration identity at the
  store boundary. #256 adds the schema field, preserves old rows as explicit
  legacy-unknown values, and rejects missing identity on every new write.
- A cache lookup before re-resolution would keep serving a token after GitHub
  stopped reporting the installation. Resolution now precedes every lookup;
  the regression removes the installation between two calls and proves the
  second fails without another mint.
- A registration can change locally after discovery but before signing. The
  minter reloads it by numeric owner and compares the stable App ID before
  constructing the JWT; the adversarial handler swaps the registration during
  discovery and proves no token request or audit follows.
- Treating `account.id` alone as canonical left inconsistent returned objects
  representable. Resolution now also requires GitHub's `target_id` to equal the
  account ID, rejects duplicate installation IDs and oversized pages, and
  decodes exactly one JSON document.
- Codex review confirmed that owner/name-only token scoping could authorize a
  replacement repository after name reuse. #259 adds the immutable repository
  ID to the owner-approved trust profile and its digest; mint requests now use
  only that ID, verify the returned ID and name, and bind the cache to the ID.
- Codex's next pass confirmed that validating only the narrowed token response
  would still permit an underlying all-repositories installation. Resolution
  now requires the installation object's `repository_selection` to be exactly
  `selected`; missing, `all`, and unknown values share a closed auditable reason
  and never reach token minting.

**Rejected by verification:**

- Unknown-owner fallback and registration-order selection: zero matches return
  `ErrNoInstallation`, while two public registrations matching the same owner
  return `ErrAmbiguousInstallation`.
- Cross-registration cache collision: two registrations with the same
  synthetic installation ID mint once each and retain distinct registration
  identities.
- Response or credential reflection through resolution errors: the typed
  mismatch test injects an attacker-controlled account login and proves it is
  absent from the error; response bodies are never wrapped, and redirects stay
  disabled by the shared client boundary.
- Minting before repository trust: a missing current profile returns
  `ErrTrustProfileDrift` with zero GitHub requests.

**Accepted by decision:**

- Resolution enumerates all registrations and fails the whole decision if any
  registration cannot be completely validated. This trades availability for
  the required fail-closed posture; silently skipping one would make a
  duplicate or malformed match disappear from the decision set.

Revisit when #248 promotes canonical numeric installation-to-repository
bindings: resolution should consume that stronger recorded coordinate without
weakening the live App/account revalidation added here.
