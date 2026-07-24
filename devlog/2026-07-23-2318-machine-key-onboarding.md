# Machine-Key Onboarding

Issue: #248.

## Decision

The publish package supplies a credential-prerequisite state machine rather
than adding `setup`, `onboard`, or `doctor` command parsing. Issue #238 owns
those operational commands; this unit returns typed registration,
key-generation, installation, and ready steps that its packaging layer can
render.

A new machine must receive the non-secret registration identity from durable
control-plane state. This unit does not invent host enrollment or a second
registration store: movable-control-plane issue #266 owns that transport. The
downloaded PEM is bounded, parsed as exactly one PKCS#1 RSA key, authenticated
against `/app`, checked against the recorded numeric App ID and canonical
metadata, and independently checked for public/private visibility before the
keystore can replace local credentials. The recorded key ID remains the
SHA-256 public-key fingerprint GitHub displays.

Doctor takes the expected non-secret registrations explicitly so it can report
a missing key on a machine whose keystore is empty. It treats forge/API
failures as errors rather than a healthy report. A duplicate fingerprint
across local registrations is the PEM-copy pattern one machine can detect;
cross-machine reuse cannot be inferred locally and remains an operator
contract.

## Rejected Alternatives

- Adding the Wave 2 CLI commands here was rejected because it would overlap
  #238's command ownership and make two work units edit the same scaffold.
- Treating an arbitrary PEM plus caller metadata as sufficient was rejected.
  Possession of a key proves only that it can sign; the authenticated App
  response and the independent visibility probe bind it to the expected
  registration before persistence.
- Requiring client or webhook secrets on a new machine was rejected because
  GitHub does not reissue them with an additional private key and publication
  authentication does not use them.

## Refute-First Verification

- **Confirmed and fixed:** an in-place key rotation initially replaced the
  one-time manifest client and webhook secrets with empty values because an
  additional-key download does not reissue them. Import now preserves those
  fields after proving the existing local registration is the same one.
- **Confirmed and fixed:** authenticated and public App probes initially
  decoded the first JSON value without rejecting trailing content. Both now use
  the package's exactly-one-document response decoder.
- **Confirmed and fixed:** accepting any non-empty slug allowed path-relative
  values to alter the emitted key-settings URL. Registration validation now
  enforces GitHub App slug length and character rules before any URL is built.
- **Confirmed and fixed:** the installation wait initially treated a janitor
  pass's temporary coverage clear as terminal even though the always-on loop
  clears coverage before every exhaustive cycle. It now waits through both
  absent installations and inactive coverage, without bypassing either gate.
- **Confirmed and fixed by automated review:** the janitor-inactive-as-terminal
  class recurred in `Next`, the sibling of the installation wait fixed above.
  `Next` handled only a confirmed absent installation, so a temporarily inactive
  janitor (starting up or mid-pass) surfaced a fatal error instead of the
  install/resume step. `Next` now routes `ErrJanitorInactive` to the same
  not-ready step as `ErrNoInstallation`, symmetric with the wait. Sweeping the
  remaining `ResolveRegistration`/`Resolve` consumers: minting still fails
  closed on an inactive janitor (operating, not onboarding) and doctor surfaces
  it as a distinct finding, so both are correct as-is.
- **Confirmed and fixed by automated review:** the onboarder trimmed trailing
  slashes only for its own URL builders and passed raw base URLs into the
  registrar and resolver, so a trailing-slash configuration produced doubled
  separators (`...//settings/apps/new`) on the registration and installation
  paths. Normalization now lives in each leaf constructor that concatenates a
  base with an absolute path (registrar, installation resolver, minter), with
  the onboarder normalizing once and sharing the result; a trailing-slash
  regression test pins the emitted URLs.
- **Confirmed and fixed by automated review:** onboarding initially used
  all-registration owner resolution after an operator selected one
  registration. Another App's installation could therefore satisfy the
  prerequisite while the returned step still named the selected App.
  Onboarding and its poll now use registration-scoped resolution.
- **Confirmed and fixed by automated review:** doctor initially checked
  janitor activity only for public registrations even though runtime
  resolution and the plan require coverage for every registration. Public and
  private regressions now pin the same finding.
- **Confirmed and fixed by automated review:** `pem.Decode` skips bytes before
  the first PEM block, so an import with a non-whitespace prefix initially
  passed the exactly-one-key check. Import now requires the trimmed input to
  begin with the RSA private-key marker before decoding.
- **Confirmed and fixed by automated review:** doctor initially continued to
  authenticate registrations after proving that they shared one key. A real
  forge rejects at least one such JWT, replacing the structured reuse finding
  with an API error. Doctor now skips remote verification for every
  registration using a duplicated fingerprint until the operator provisions
  distinct keys.
- **Rejected by verification:** a mismatched App ID, visibility mismatch,
  malformed, prefixed, or trailing PEM content, widened keystore mode, missing
  key, inactive public-registration janitor, or legacy singleton layout can
  reach a ready state. Fake-forge flow tests and focused race tests cover these
  cases; all fail closed or produce the specified doctor finding.
- **Accepted residual:** a copied PEM used on another host cannot be detected
  from local state alone. Doctor reports duplicate local fingerprints; the
  portable registration/key inventory that can detect cross-host duplication
  belongs to #266.

Revisit when: #266 defines the durable registration inventory and host
enrollment transport consumed by this prerequisite.
