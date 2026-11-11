# Tests for Two Untested Credential Gates

Decision record for #3052, deferred from #2864
(`devlog/2026-11-10-1912-freesided-start-function.md`). #2864's
refute-first mutations found two Tier A gates in
`daemon/internal/daemoncompose` that no test checked. This unit adds a
test for each and changes no production code. The note is mandatory
because the tests check Tier A gates.

## The Gates and Why Each Is Reachable

- **The integrity probe's rig authorizer.**
  `ProductionCredentialIntegrityObservers` observes setup-token volumes
  through `ProductionRigRuntimeAuthorizer`. The authorizer records the
  observer container's name in the held rig manifest before the
  runtime creates it, which is how rig cleanup finds an observer a
  crash leaves behind. Without it the create still happens, so nothing
  but the manifest shows the gap.
- **Codex discovery's auth-method filter.** `modelCatalogRefresh.targets`
  takes a Codex CLI enrollment only on OAuth. Neither
  `domain.ClientEnrollment.Validate` nor the store refuses a Codex CLI
  enrollment on a setup token: the new test seeds one, with a current
  generation, through the ordinary store writes. So the filter alone
  keeps discovery from reading that enrollment's credential.

## How the Tests Catch Each Mutation

Chose to assert on the observable effect each gate exists for over
asserting that the gate was called.

- `TestProductionCredentialIntegrityObserversBindSetupTokenObserver`
  holds a real rig, drives the probe's `SetupToken` against a fake
  runtime that refuses the create, and checks that the name the runtime
  was asked to create is in `daemonlock.ReadRigManifest`. Reading the
  name from the create request keeps the test independent of the
  observer's name prefix.
- `TestModelCatalogRefreshSkipsCodexEnrollmentsNotOnOAuth` seeds a
  Codex OAuth enrollment beside a Codex setup-token one and checks that
  only the OAuth one gets a credential read, a discovery call, and a
  catalog. The OAuth enrollment keeps the test from passing when
  discovery reads nothing at all.

## Mutation Results

Each mutation was applied alone, run against the new test, and
reverted, leaving the production file unchanged.

- **Authorizer replaced with `ward.RuntimeResourceAuthorizer(nil)`** in
  `credential_integrity.go`: the probe test failed with an empty
  bound-container list.
- **Auth-method clause dropped** from the Codex target in
  `model_discovery.go`: the Codex test failed, with credential reads and
  discoveries for both enrollments instead of only the OAuth one. The
  other model-catalog refresh tests still passed, as #2864 found.

Revisit when either gate moves or changes shape; the tests name the
functions they drive.
