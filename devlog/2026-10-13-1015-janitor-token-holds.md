# Wait Out Unaccountable Janitor Tokens Before Minting Again

Work unit: #1997. Scope: `daemon/internal/publish/`, `daemon/cmd/freesided/`,
`app/` fixtures and tests, and this note.

## Owner Decisions

On 2026-10-13 the owner chose an installation-specific mint hold over an
immediate daemon stop for an unknown mint outcome. The pass still stops, but
its cause reaches the supervisor: a 5xx or network failure counts toward the
existing three-failure threshold. A later pass that only waits for a hold
neither increments nor clears that count. Other registrations can regain
coverage on that pass.

The hold lasts 69 minutes from the mint request: GitHub's one-hour token
lifetime plus the existing nine-minute skew allowance. No new token is minted
for that installation during the hold. This revises the fatal-loop decision in
[the registration-isolation note](2026-07-25-1126-janitor-registration-isolation.md):
the hold prevents the repeated mints that made continuing unsafe. It also
revises [the bounded-expiry note](2026-07-31-0930-bounded-token-expiry.md): the
owner accepts the documented lifetime as the bound even when the daemon could
not inspect the token's expiry. Returned grants and expiries are still checked
before a known token can be used. Audit failures and unknown outcomes with a
non-transient cause still stop the daemon immediately.

The owner included failed revocations in the same policy. A known token stays
in memory as a `Secret` solely to retry revocation at the start of the next
pass. A successful revoke ends the hold early. Another failed revoke counts
as a new failure, without extending the original expiry deadline or minting
another token. The hold survives the janitor loop ending within one process;
it does not survive daemon restart.

The owner also chose separate durable-stop cards over replacing the first
cause. Each distinct cause retains its own item and therefore receives its
own per-device notification. An identical open reason is deduplicated for
write retries. A new stop strips Resume Unattended from every existing stop
item; a fresh daemon start offers recovery on all of them. The daemon's reason
explains that Acknowledge and Run Doctor leave admission closed, and recovery
requires restarting and choosing Resume Unattended on each stop item.

## Rejected Options And Costs

- Keeping the immediate stop and only explaining it leaves an ordinary
  external outage dependent on a human and blinds published-PR observation.
- Closing admission while the janitor keeps watching introduces a separate
  lifecycle policy that the selected hold does not need.
- One card listing every cause retains notification deduplication against
  the same item, so later stops do not notify devices again.
- Persisting the hold needs a store migration and a separate contract unit.
  A restart inside the hold can mint again, as it could before this change.
  Follow-up: #2004.

A held registration cannot serve the active-resource watch. Four observation
failures raise the advisory `active_resource_observation_failed` item, which
resolves once observation recovers. The resolver recovery test covers the gate
where the exit run lost merge observation; it does not run a live GitHub
outage or the entire pull-request watch.

## Refute-First Findings

Independent review found no confirmed defect. It identified two boundary
cases now pinned by tests: a rejected expiry joined with a revoke 500 still
stops immediately, and repeating an existing cause strips recovery from
every stop item without creating a third card. Overlay checks against the
old code make the hold, resolver recovery, failure-counting, separate-card,
delivery, and recovery-wording regressions fail.

An expired retained `Secret` can remain in memory if its installation never
reaches enumeration again. The accepted lifetime bound removes its authority
at that deadline; review found no live-credential harm that justified a
separate cleanup mechanism. Losing holds across restart remains allowed by
the owner's explicit scope decision, with persistence tracked in #2004.

Revisit when GitHub changes the installation-token lifetime, a restart inside
a hold proves consequential, or three failures on a 30-second cadence prove
too short an outage to justify stopping.
