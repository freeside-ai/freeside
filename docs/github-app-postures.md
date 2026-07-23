# GitHub App Postures

Freeside gives each operator their own GitHub App identity. Registrations,
private keys, and bot attribution are never shared between operators. The
default and work-account postures use the same repository trust profiles,
least-authority token requests, and fail-closed mint checks; the difference is
who owns each App registration and where it may be installed.

## Default Public Posture

The default is one public App registration owned by the operator's personal
GitHub account. The App may be installed, with repository-scoped selection, on
the operator's account and on organizations that approve it. One registration
therefore follows the operator across repository-owning accounts without
multiplying App administration and key provisioning.

Public does not mean that an installation is trusted. Freeside:

- keeps publication credentials outside agent workspaces;
- refuses to operate the public registration until its always-on installation
  janitor has completed a successful reconciliation pass;
- mints only for a repository and owner already admitted by trusted local
  state; and
- polls the registration's installations, leaves trusted owners untouched,
  and audit-records then removes installations belonging to unknown owners.

The janitor performs bounded removal work per cycle. If it stops, cannot read
the complete installation set, or cannot write its audit record, public-App
operation fails closed. An unsolicited installation never authorizes a token
mint and never creates an attention item.

## Residual Tradeoffs

The public posture deliberately accepts a visible and time-bounded surface:

- The App's name and landing page are public.
- Outsiders can create unsolicited installations. The mint gate denies them
  authority immediately, but they create cleanup churn until the next janitor
  pass removes them.
- One registration's machine keys address every installation granted to that
  operator. Repository-scoped token minting limits ordinary Freeside use, but
  a stolen App key has a wider blast radius during the interval before grant
  reconciliation responds.
- The janitor narrows Freeside's authority and lifetime of unknown
  installations; it cannot make a public registration undiscoverable or erase
  the detection window.

Every machine uses its own separately revocable App key. Copying a PEM between
machines defeats localized revocation and is outside the supported contract.

## Work-Account Opt-In (Phase 1B)

Choose the private work-account posture when an organization must own and
terminate the credential, policy rejects a publicly visible App page, or the
smaller account-local key blast radius is worth the extra administration. In
that posture, each repository-owning account has a private App registration
for the operator. It requires separate registration, permission evolution, and
per-machine key provisioning for each account.

Issue #252 tracks the operator flow for this specified but not yet wired
posture. When enabled, it is not a bypass: private registrations retain the
same repository trust, installation-token, audit, and complete
grant-reconciliation gates as the default posture.
