# Promote A Pending Installation Before The Image Build

Issue #3437 found that an onboarding approval could fail only because the
image build outlasted the installation intent: `Onboard.Run` promoted the
pending installation after the build, and by then the intent had expired, so
the approved review was lost. This note records why promotion moved and what
the move costs. It is mandatory: promotion changes which installation is
trusted (Tier A, approvals and GitHub tokens).

## Decisions

- **Promote at approval, before any image build or reuse, when no trust
  profile covers the repository.** Chose this over promoting after the
  build. The approval digest already binds the pending installation, its
  expected account, and its repositories, so the build adds nothing
  promotion needs to wait for. Promoting first means expiry is
  checked once, at the moment of approval, and a long build can no longer
  void an accepted review. After promotion the run waits, bounded at one
  minute, for the janitor to cover the repository under the trusted binding,
  because the onboarding token source takes the trusted path once the
  binding is trusted and needs that coverage to mint the build's read token.
  It rechecks the trusted installation again before activation, as before.
- **Early promotion grants no runtime authority.** The runtime minter
  requires a current trust profile for the repository, and activation still
  comes last. A promoted installation for a repository with no profile can't
  mint a runtime token, publish, or reach attention. The one token it can
  mint before activation is `freesided preflight`'s read-only inspection
  token, which needs only a trusted binding; it reads the repository the
  owner approved, which onboarding's own audit token already read.
- **Keep the old order for a repository that already has a profile.** The
  minter checks only that the repository has a current profile and that the
  janitor covers a trusted binding for the profile's repository ID; the
  profile names no installation. A repository can hold a profile while its
  installation is pending again, after a quarantine with
  `-recover-installation` or after the janitor removed its installation.
  Promoting early there would let the old profile mint through the new
  binding for the whole build, and indefinitely after a failed one, a
  pairing the owner never approved. So that case promotes after the image
  is recorded and activates at once, with no coverage wait or recheck in
  between, exactly as before; it keeps the old exposure to a build that
  outlasts the intent. A profile counts when it names the repository's ID
  or, case-insensitively, its name, and a profile that no longer reads
  counts too, since a false match costs only the old order. Found by the
  fresh-context review.
- **Let exit-root review again after an approval that did not complete.**
  The script stops for a recorded review's approval without reviewing
  again, but a failed build after early promotion leaves that approval's
  digest stale, so the script would reprint a command that can never
  succeed. An approval that ran leaves `onboard-approved.json` behind its
  redirect without a complete status, and the script now reviews again on
  that sign, and on `--replace-intent`.
- **Reject extending or freezing the intent's expiry.** Plan §GitHub App
  Agent Identity bounds the janitor's exception for an unsolicited-looking
  installation by the intent's expiry. Extending the expiry during a build,
  or freezing it at approval, would stretch that exception by however long a
  build takes, which the plan does not allow. Promotion ends the need for
  the exception instead: the installation becomes trusted and the janitor
  keeps it on that basis.
- **Accept that a failed build after promotion needs a fresh review.** The
  installation stays trusted and no profile is active. A rerun now reviews a
  trusted installation, whose approval digest differs from the pending one
  (the digest includes the pending installation), so the operator approves a
  new digest. Chose this over rolling back the promotion, which would need a
  demotion path that the plan does not have and that would itself be a
  destructive trust change.
- **Keep the janitor's listing diagnostic only.** A `-resume` that waits
  until expiry now names the installations the janitor's last pass listed for
  the registration, so the operator can see a wrong account or repository
  set. The listing is published with the pass and cleared when the janitor
  stops; nothing reads it for coverage, readiness, or a mint.

## Refute-First Findings

- **Confirmed and fixed:** early promotion with an existing profile, the
  profile check keyed by name alone, and a late path that waited and
  rechecked between promotion and activation (all above), and exit-root
  ignoring `--replace-intent` once a review was recorded. The recheck before
  activation, which now also runs after early promotion, compared account
  logins case-sensitively, so an intent opened with a different spelling of
  a trusted login failed after a full build; it now compares them
  case-insensitively, the account ID being the identity. The recovery
  command put `-replace-intent` after a trailing repository, where the flag
  parser refuses it; it now goes before the flags or after a leading
  repository.
- **Disproved by trace:** the coverage wait is bounded. The onboarding
  gate's `AwaitAllowsRepository` waits only for a janitor pass already
  running, and the janitor clears that pass between passes, so each check
  returns after at most one pass and the one-minute deadline holds, give or
  take one pass.
- **Accepted:** the preflight inspection token above can mint for a
  promoted installation with no profile, after a failed build or a coverage
  timeout. It is read-only and scoped to the approved repository.
- **Also disproved by trace:** an expired intent still authorizes no token and
  can't be promoted (`promote` checks expiry before writing, and
  `PromoteInstallation` and the onboarding token source refuse an expired
  intent). The janitor's listing is swapped whole under its lock, copied on
  read, cleared when the janitor stops, and read by no coverage, readiness,
  or mint path.

Revisit when the plan gains an installation demotion path, or when builds
routinely fail after promotion often enough that the second review costs
more than the expiry it avoids.
