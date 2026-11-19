# Accept Codex 0.167 Auth Files by Modeling Their Two New Token Members

**Chose to model `tokens.oauth_provenance` and `tokens.login_owner_id` as
optional members of the strict `auth.json` decoder, and to accept a
provenance only when it names Freeside's own refresh issuer and client, over
tolerating unknown members, ignoring the two by name, or following the
recorded issuer, because each alternative either weakens the strict decoder,
drops data on rotation, or lets the file choose where a refresh token goes.**
#3449 carries the contract; #2382's exit run found the refusal (friction log
H2), and that run pinned the 0.162.1 CLI to get past it.

## What Changed in Codex

Codex 0.167 and later add two optional members under `tokens`
(`codex-rs/login/src/token_data.rs` at openai/codex
`692292695bbacbd3e39eb3f3bf36129a94712806`):

- **`oauth_provenance`**, an object with `issuer` and `client_id`. Codex
  posts the refresh token to `<issuer>/oauth/token` with that client id.
- **`login_owner_id`**, a string Codex preserves across refreshes.

Codex writes both only when set and reads `null` as absent. Freeside's
`inspectCodexHostAuth` decodes strictly, so both read as unknown and every
path that inspects the host store refused it: enrollment, renewal, proactive
refresh, and review readiness.

## Decision

- **Model the members, don't tolerate unknown ones.** The strict decoder is
  what refuses a shape Freeside doesn't understand; relaxing it for all of
  `tokens` would let the next unmodeled credential-bearing member through.
  Any other unknown member, at the top level, in `tokens`, or in
  `oauth_provenance`, is still refused.
- **Model them, don't skip them by name.** The refresh transaction and the
  reviewer's access-only snapshot re-encode the decoded struct. A member the
  struct doesn't hold would vanish on the first rotation, and Codex would
  then refresh a store that has lost its provenance. Pointer fields with
  `omitempty` keep a pre-0.167 store from gaining either member.
- **Pin the provenance; don't follow it.** Freeside always refreshes against
  `https://auth.openai.com/oauth/token` with client
  `app_EMoamEEZ73f0CkXaXp7hrann`. A store whose provenance names another
  issuer or client was issued by an authority Freeside doesn't refresh
  against; sending its refresh token to OpenAI would be wrong, and following
  the file would let untrusted input choose where a credential goes. So a
  provenance must have a non-empty issuer and client id, its issuer with
  trailing `/` trimmed must equal the refresher's issuer, and its client id
  must equal the refresher's. The issuer is now one constant the endpoint is
  built from.
- **Treat the new strings as agent-visible.** `login_owner_id` and both
  provenance strings reach the reviewer's snapshot, so the refresh-token
  aliasing gate and the rotation checks cover them as they cover the ID
  token, access token, and account id.

## Refute-First Findings

A council of two fresh-context reviewers (Codex `gpt-6.2-astra` and Claude
Opus 5.5, high effort) tried to prove the change wrong at its first
revision. Neither found a defect that blocks it.

- **Case-variant member names are accepted.** Confirmed, allowed by an
  explicit decision. A single `"ISSUER"` or `"LOGIN_OWNER_ID"` decodes into
  the modeled field and is rewritten in canonical spelling. `strictjson`
  keeps v1 case-insensitive name matching on purpose
  (`devlog/2026-11-08-1212-json-v2-strict-decoding.md`), it already applies
  to every existing `auth.json` member, and a case-variant duplicate is still
  refused. The pin checks the value whatever the key's spelling, so no
  spelling sends the refresh token elsewhere. Codex raised it as blocking;
  Claude called it pre-existing.
- **A refresh token that is a substring of the pinned issuer or client id
  is refused as aliased.** Confirmed, allowed: it fails closed, and real
  refresh tokens are far longer.
- **An explicit `null` member is dropped on re-encode.** Confirmed, allowed:
  Codex reads `null` as absent, so the meaning is unchanged.
- **The pin matches Codex's own rule,** by the Claude reviewer's reading of
  the cited Codex commit. Codex keeps a provenance only when
  its endpoint equals the issuer with trailing `/` trimmed plus
  `/oauth/token` (`codex-rs/login/src/auth/manager.rs`), the normalization
  the pin uses.

## Revisit When

- A real Codex file carries a provenance with another issuer or client id.
  That is an owner decision about following the provenance, not a reason to
  loosen the pin.
- Codex adds another member under `tokens` or at the top level. It is refused
  until modeled, by design.
- The reviewer image moves past a Codex version whose `TokenData` denies
  unknown fields; the 0.162.1 image tolerates the two members.
