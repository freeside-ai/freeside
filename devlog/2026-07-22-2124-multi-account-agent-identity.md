# Multi-Account Agent Identity

Issue: #244. Follow-up initiative: #251.

## Decision

Chose one agent principal per Freeside operator, with no registration or
private-key sharing across operators. A logical principal may have several
GitHub App registrations, but each registration's numeric App ID is bound to
exactly one principal and is the credential identity used for trust. App names,
slugs, and bot logins are display metadata only.

Chose registration topology as owner policy rather than architecture. The
default is one public App owned by the operator's personal account, installed
separately on each personal or organization account through GitHub's native
approval flow with repository-scoped selection. The supported opt-in
work-account posture is one private App per repository-owning account for an
operator whose organization must own and terminate the credential. The opt-in
does not create a shared organization principal: another operator receives a
different registration and keys.

The topology choice does not alter three unconditional protections. Token
minting fails closed unless the target repository is onboarded and trusted and
the specific installation is recorded as known for that repository under a
known registration bound to the principal. The request carries exactly the
target repository's canonical numeric ID and the profile-approved permissions.
The returned token object is not trusted until its repository set, permissions,
and bounded expiry are verified; a mismatch discards the token before worker
exposure. This one-repository rule governs worker-bound tokens. The janitor's
separate full-grant read credential omits `repository_ids`, requests only the
minimum permissions accepted by GitHub's list-repositories endpoint, remains
in daemon memory, can call only that paginated endpoint, and is revoked on
completion or failure without worker, disk, or log exposure. The janitor mints
it only after App-authenticated metadata proves the installation has a trusted
binding or matches an unexpired pending envelope's registration, expected
owner, installation ID when known, and current lease generation, with a trusted
owner additionally required for a public trusted binding. It deletes every
other installation from that metadata alone, without pulling its repository
list. Pending repository IDs are not asserted until enumeration returns the
complete set; only an exact match then receives the temporary reconciliation
exception. GitHub inputs can
cause AttentionItems only through a trusted principal and recorded
installation-to-repository binding, with no unauthenticated GitHub write path
into the attention system, so an unsolicited installation or repository grant
has no path into it. Daemon- and system-originated items remain governed by
their own contracts. An always-on janitor enumerates every installation and its
granted repositories for every registration. It treats the returned pages as
untrusted until pagination completes, then compares the canonical repository
ID set. Before that comparison, the canonical installation response must report
`repository_selection: selected`; a missing or different mode is drift even
when today's IDs match. An installation survives reconciliation only with a
trusted binding or an exact, unexpired pending-install-or-expansion intent. A
trusted public binding also requires a trusted owner; an exact pending intent
temporarily exempts only its expected owner and exact post-change repository
set. Outside that no-authority exception, any unrecorded repository grant or
non-selected repository mode suspends and audit-logs the entire affected
installation. Suspension is
terminal quarantine: Freeside records the observed grants, deletes the
installation with App authentication, invalidates its binding, and requires a
fresh native installation. It never automatically unsuspends or mints against
the drifted installation. The janitor is a
prerequisite for operating every registration; visibility is not a trust
decision.

Before routing an operator into GitHub's native installation, organization
approval, or selected-repository expansion flow, onboarding records a
single-use, expiring pending intent with the registration, expected numeric
owner, installation ID when known, current trusted repository IDs, exact
post-change repository IDs, required selected-repository mode, and callback
nonce. That record grants nothing beyond existing trusted bindings; the added
repository has no mint, publication, attention, or principal authority. The
janitor may temporarily retain exactly one matching remote installation. A
same-session callback verifies the nonce but is only an acceleration. The
daemon also polls App-authenticated installation state, and an explicit local
resume reopens the intent after another browser, session, or daemon restart.
Either path moves an exact canonical match only to ready-for-review; acceptance
of the local one-time trust review atomically promotes it to a trusted binding.
Until then, only the intent's expected owner and exact post-change repository
set bypass the janitor's untrusted-owner and unrecorded-grant branches. This is
a reconciliation exception, not authority, and it ends on promotion or expiry.
Expiry, replay, ambiguity, a non-selected mode, or an over-broad grant fails
closed into ordinary reconciliation.

Chose to keep GitHub's native installation or organization approval as an
explicit account-onboarding prerequisite rather than pretending Freeside can
collapse an external administrator's decision into its one manual repository
review. `freesided onboard` routes to that native flow when required, waits,
and resumes the same onboarding transaction against the approved installation.
The Phase 1A thirty-minute, one-manual-step repository target applies once that
prerequisite is complete; platform approval latency is measured separately.

Chose one private key per machine within a registration, recording the SHA-256
public-key fingerprint GitHub displays in App settings, over copying a PEM
between machines or inventing a local-only key ID. The fingerprint identifies
the exact key the operator must delete for a lost, retired, or compromised
machine. Keys are individually revocable, so one deletion does not rotate every
machine. Multiple daemons may act concurrently as the same principal; no
reconciliation, idempotency, or publication rule may assume one writer per
principal.

Chose a principal-wide compare-and-swap ledger for registration bindings,
pending intents, and expiring installation-mutation leases over independent
per-daemon pending records. The lease is keyed by registration plus owner until
GitHub supplies the installation ID, then by registration plus installation.
It carries a generation and base binding-set version. Only its current holder
may redirect, resume, promote, or release; a competing daemon attaches or
waits. If the shared authority is unavailable, multi-machine installation
mutation fails closed. The concrete backing is an implementation choice, but a
local SQLite row cannot satisfy this contract across machines.

Chose generated but non-authoritative App names. GitHub App names are globally
unique and limited to 34 characters while account names can reach 39, so the
suggestion truncates the account-derived portion while preserving a legible
username, reserves room for a numeric suffix, and retries collisions with
increasing suffixes. The manifest conversion response is canonical for App ID,
name, and slug because the registration page may edit the suggestion and
GitHub resolves uniqueness. Renaming later is outside ordinary operation
because it churns the visible `[bot]` login; numeric App ID remains stable trust
identity regardless of display names.

Daemon-authored agent commits carry a `Co-authored-by` trailer using
`<bot-user-id>+<app-slug>[bot]@users.noreply.github.com`. Freeside resolves and
records the bot account's numeric user ID from the canonical App slug. The
trailer makes the principal legible in history but only complements
credential-level attribution; the bot user ID and slug grant no authority and
substitute for none of the App-ID, installation, repository, or mint checks.

## Rationale

The public default turns a new repository-owning account into the familiar
GitHub install-or-request-approval action. One registration, one initial
provisioning flow, and one permission update serve a user's accounts, while
each organization retains installation approval, repository scoping,
suspension, and uninstall control. This avoids multiplying registration,
permission-evolution, and key-provisioning work by every organization an
operator uses.

GitHub's current platform boundaries support the choice: a private App can be
installed only on its owning account, while a public App can be installed on
other accounts; installation selects account and repository scope. The
manifest conversion returns the canonical App record and initial key, and a
registration supports multiple separately revocable private keys. GitHub's
documented App-name limit is shorter than its account-name limit, so
truncation and collision handling are contract requirements rather than edge
polish.

The default accepts a bounded residual surface: a public registration has a
public name and landing page, outsiders can create unsolicited installations,
and one registration's keys span the repositories authorized to that
principal. A trusted owner can also widen an installation's repository grant.
The mint gate contains authority inside Freeside immediately; the janitor
deletes unknown installations and terminally quarantines then deletes known
installations whose repository grants drift, but cannot erase the window before
its next pass. Operators whose governance rejects that residual use the
work-account posture.

## Rejected and Demoted Alternatives

- **Fine-grained personal access tokens:** rejected because they forfeit the
  GitHub App bot identity and short-lived installation-token model.
- **One shared multi-key App:** rejected because a shared registration destroys
  per-user attribution, localized operator revocation, per-user repository
  scoping, and per-user rate budgets. Per-machine keys distinguish machines,
  not people, and do not repair a shared principal.
- **Private App per repository-owning account as the default:** demoted to the
  work-account opt-in. It preserves organization ownership and termination but
  multiplies onboarding, permission updates, registration administration, and
  key provisioning per operator per account.
- **Names or slugs as identity:** rejected because they are mutable display
  values and globally collision-prone. Numeric App ID is the stable trust key.

## Refute-First Findings

- **Confirmed:** public visibility expands who may create an installation. The
  mint gate prevents authority from that installation before the janitor sees
  it, the attention invariant prevents it from creating a human-decision path,
  and the janitor removes it. The interval and churn remain accepted residuals,
  not evidence that public visibility itself is a trust boundary.
- **Confirmed:** an administrator of a trusted owner can widen a selected-repo
  installation or grant all repositories. The latter is future authority even
  when the owner currently has no unrecorded repository, so ID-set equality is
  insufficient. Freeside's mint gate rejects extra repositories, but a stolen
  App private key can bypass the daemon and request tokens directly. The
  janitor therefore requires selected-repository mode and compares the complete
  remote grant set with recorded installation-to-repository bindings. Any mode
  or ID drift suspends the installation, records the evidence, then deletes it
  and requires a fresh install. GitHub's App-authenticated API can suspend or
  delete an installation, while removing one repository requires a separate
  user credential. Terminal quarantine preserves the no-PAT design and avoids
  reopening the compromised-key window merely to verify recovery. The interval
  before detection remains an accepted residual. The same drift and response
  exist for public and private registrations.
- **Confirmed:** omitting `repositories` or `repository_ids` when minting gives
  the token every repository granted to the installation, and omitting
  `permissions` gives it every App permission. A trusted multi-repository
  installation therefore still needs a one-repository, least-permission mint
  request. The response is a returned-object trust boundary: Freeside verifies
  the exact repository ID, a sufficient but non-escalated permission set, and
  bounded expiry before exposing the token. A missing field or mismatch is a
  hard failure, not a reason to trust the request intent.
- **Confirmed:** a one-repository token cannot prove that an installation has
  no surplus grants because GitHub's list-repositories endpoint reports only
  repositories accessible to that token. The janitor therefore needs the sole
  exception to the repository filter. Its token carries the minimum permission
  set for that read endpoint, stays inside the daemon, is capability-limited to
  complete paginated enumeration, and is revoked immediately. The returned
  pages are another returned-object trust boundary: incomplete pagination,
  duplicate or malformed IDs, or a comparison failure cannot produce a clean
  reconciliation verdict.
- **Confirmed:** minting that full-grant enumeration token before checking the
  App-authenticated owner and binding metadata would unnecessarily retrieve an
  outsider's private-repository metadata after an unsolicited public-App
  installation. The janitor deletes installations with neither a trusted
  binding nor a metadata-matched unexpired pending envelope directly from
  App-authenticated metadata; only candidates that pass that gate can receive
  the enumeration token.
- **Confirmed:** App-authenticated installation metadata does not carry the
  selected repository ID set. Requiring an exact pending repository match
  before minting the enumeration token is therefore circular. The pre-token
  gate matches only the trusted binding or pending envelope fields available in
  App metadata; the returned complete repository set must then match the
  pending post-change set before the reconciliation exception applies.
- **Confirmed:** native installation, organization approval, or expansion of an
  existing installation to a new repository can complete between janitor polls
  and the onboarding callback. Without prior state, the legitimate change is
  indistinguishable from an unsolicited installation or grant. The bounded
  pending intent is written before the redirect, matches only the expected
  owner, installation when known, selected mode, and exact post-change set,
  and grants no new authority. Callback nonce, App-authenticated polling, or an
  explicit local resume can make the exact match reviewable; none substitutes
  for the accepted local trust review that atomically promotes it. The intent
  temporarily exempts only its expected owner and exact post-change repository
  set from deletion or quarantine; all mint, publication, attention, and
  principal authority remains denied until promotion. The exception
  expires into normal reconciliation. The remote grant still exists during that
  bounded window, so a stolen App key could address it directly; that
  user-initiated residual is accepted rather than making native approval or
  repository onboarding impossible to complete.
- **Confirmed:** two daemons can otherwise start repository expansions from the
  same trusted repository set, producing a remote union that matches neither
  exact pending intent and causing terminal quarantine of a legitimate
  installation. Principal-wide CAS admits exactly one mutation lease per
  installation (or registration-owner pair before an installation ID exists).
  A loser attaches or waits before any redirect; it cannot create a second
  remote mutation from the stale base version.
- **Confirmed:** suspension blocks the App from the installation account, so a
  suspended installation cannot mint the enumeration token needed to verify a
  narrowed grant. Automatic unsuspension would briefly restore the same remote
  authority the quarantine is meant to contain. Drift recovery therefore
  deletes the installation and re-enters the pending-install flow; the old
  installation is never automatically unsuspended.
- **Confirmed and accepted by decision:** one public registration's private
  keys span every repository installation authorized to that principal. Exact
  repository selection and the fail-closed mint gate bound use, but cannot
  make a stolen key account-local. The work-account opt-in is the smaller
  blast-radius posture for owners who require it.
- **Confirmed and accepted by decision:** GitHub attributes actions to the App,
  not to an individual private key, so two machines acting as one principal
  share the same remote bot identity. Local fingerprint records identify the
  key to revoke and audit which credential Freeside selected; they do not claim
  remote per-key attribution. Same-principal operations remain concurrent
  writers.
- **Confirmed:** a local-only key ID cannot tell an operator which key to delete
  in GitHub. GitHub displays the key pair's stable SHA-256 fingerprint in App
  settings, where private-key deletion is performed. Freeside records that
  fingerprint per machine and uses it in revocation instructions; it does not
  claim GitHub exposes per-key action attribution.
- **Rejected by the contract:** a public installation can become a principal,
  mint against an untrusted repository, mint merely because its registration
  is known, or inject an AttentionItem merely by existing. Principal bindings,
  repository onboarding, a recorded installation-to-repository binding, and
  the lack of an unauthenticated GitHub-to-attention write path are independent
  gates before those effects. This restriction does not apply to daemon- or
  system-originated items.
- **Rejected by the contract:** a name, slug, bot login, or commit trailer can
  grant authority. Only numeric App ID and the registration, installation,
  repository, and token-mint bindings participate in trust decisions.
- **Rejected by first-party verification:** the numeric App ID can populate the
  GitHub bot's no-reply address. GitHub's maintained App-token action resolves
  `/users/<app-slug>[bot]` and uses that bot account's distinct numeric user
  ID; App ID remains trust identity while bot user ID remains attribution
  metadata.

## Verified Platform Assumptions

The decision was checked against GitHub's first-party documentation on
2026-07-22: public versus private installation scope; native account approval
and repository selection; the 34-character globally unique App-name rule; the
manifest conversion response; separately managed private keys; and the
App-authenticated installation-token, deletion, and suspension endpoints. A
token request without `repositories` or `repository_ids` covers every granted
repository and one without `permissions` carries every App permission; the
response includes repositories, permissions, and expiry for verification. The
canonical installation response also reports `repository_selection`, which is
checked independently of the current ID set. The installation-token
list-repositories endpoint requires no additional App permission and supports
explicit token revocation. The per-repository removal endpoint instead requires
a classic user token. Suspension blocks the App from that installation account,
while the App-authenticated deletion endpoint does not require an installation
token; the janitor therefore uses terminal quarantine plus deletion rather than
adding a user credential or automatically unsuspending. These are platform
dependencies, not Freeside guarantees. GitHub also identifies an App key pair
in settings by its SHA-256 fingerprint and performs private-key deletion there;
that displayed fingerprint is the per-machine revocation locator.
GitHub's maintained App-token action also confirmed the bot-user-ID no-reply
address after automated review disproved the requested App-ID form. A GitHub
change that invalidates one requires re-evaluating the affected topology,
naming rule, attribution carrier, mint boundary, or reconciliation response.

Automated review also exposed seventeen internal contract gaps: Section 5.5
named a known registration without requiring the specific
installation-to-repository binding, the Phase 1A one-step repository target
did not account for a missing organization installation, and the janitor
checked trusted owners without
checking repository-grant drift inside them. It also left GitHub's default
installation-token scope intact, then scoped grant reconciliation only to the
public topology, then made full-grant enumeration impossible with the worker
token rule. Finally, it left a race between native approval and the janitor and
specified an impossible verify-while-suspended recovery loop. It then compared
repository IDs without rejecting `all` selection, which silently grants future
repositories, then protected only new installations from the onboarding/janitor
race and not expansions of existing installations. It then relied on a callback
that organization approval may never return to and recorded a non-GitHub key ID
for revocation. Finally, it recognized exact pending intents but left the
untrusted-owner and unrecorded-grant drift branches unconditional, then minted
a full-grant token before rejecting an unsolicited outsider installation, then
allowed concurrent daemons to create incompatible exact-set intents, then made
the pre-token pending check depend on repository IDs available only after that
token returns. It finally found the note's opening summary still made the
trusted-owner check unconditional despite the later pending exception. The
plan now fails minting closed at the specific installation
boundary, scopes that target to repositories whose external GitHub installation
or approval prerequisite is complete, serializes native installation and
selected-repository expansion with a bounded non-authoritative pending intent,
supports callback, polling, and explicit local resume while reserving promotion
for accepted local review, exempts only the exact pending owner and repository
delta from reconciliation without granting authority, serializes installation
mutation through principal-wide CAS state, requires
selected-repository mode, terminally
quarantines and deletes any public or private installation whose mode or remote
grants drift, requires one-repository, least-permission worker token minting
with response verification before exposure, isolates the janitor's full-grant
credential as a read-only, daemon-internal, immediately revoked exception, and
stages its pre-token metadata gate before post-enumeration exact-set matching,
and records
GitHub's SHA-256 key fingerprint per machine for revocation.

## Revisit When

Revisit the public default if unsolicited-installation churn becomes material
despite the janitor, GitHub adds installation allowlists, GitHub exposes
per-key audit attribution that changes the principal design, or an
organization requires credential ownership and termination. The last case can
use the existing work-account opt-in without changing the architecture.
Choose and adversarially verify the shared CAS backing before enabling
multi-machine installation mutation; local-only state is explicitly
insufficient.
