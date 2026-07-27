# Operator Machine Provisioning and the Live Publication Smoke

Work unit: #306. Scope: `devlog/` (the provisioned state is
repo-external operator state).

## What the Operator Machine Now Holds

Cross-session context the repo cannot carry: the 1A.2 operator machine
(the one #237 will run on) is provisioned as of 2026-07-26.

- Apple `container` 1.1.0, apiserver running and launchd-registered
  (`com.apple.container.apiserver`).
- State root `~/.freeside`: `state/freeside.db`,
  `credentials/github-app/53575454/` (App 4385298, public, owner
  `bennelsonweiss`), and the operator-authored
  `state/installation-authority.json` naming registration 4385298,
  trusted owners `bennelsonweiss`/53575454 and `freeasinbird`/84958515,
  installation 148770512 bound to exactly repository 1278475858
  (`freeasinbird/gh-imgup`), epoch 1, revision 1, no pending envelope.
  Verified through the daemon's own store read path
  (`publish.NewInstallationAuthorityStore` + `InstallationAuthority`).
- Exporter image seeded in the local content store with a resolvable
  digest reference
  (`127.0.0.1:5005/freeside-exporter@sha256:c5a97603039a...afbf`, built
  2026-07-21 via `--local-registry-port`).
- Claude CLI 2.1.220 with an operator-minted setup token in the login
  Keychain (`freeside-claude-setup-token`), distinct from the
  interactive login; #237's contract test pins its scope and the CLI
  version.

## Smoke Evidence

One live attended fake-candidate publication (`freesided
-fake-publication`) ran against the real state root: janitor coverage
from the authority snapshot, token minted, base
`6ab4e3dff2be53f74bde9b8b3150290775152f9f` (gh-imgup `main`), candidate
imported and verified, published as `freeasinbird/gh-imgup#84` by the
App identity on deterministic branch `freeside/publish/c22aee448bd41d4a`,
head `a676602dbe30cc09f0ed947b1553d47f142193f0`. Audit: exactly one
commit adding `docs/freeside-attended-smoke.md` (+6/-0), inside the
declared `docs/**` allowlist; terminal attention item
`ready_for_final_review`. The candidate exists only as smoke evidence:
its pull request was never intended to merge, and its lifecycle lives
on the forge. The opt-in live publish suite passed 4/4 on this machine
on 2026-07-26, after #332's harness fix.

## Findings That Change Direction

- **`freesided -fake-publication` cannot bootstrap the fake stage
  driver's directory.** The engine's protected-root check `lstat`s
  `<db>.fake-stage-driver` before anything creates it: the driver
  itself initializes fine on a fresh root (a missing state file reads
  as empty) but materializes its directory only on the first persisted
  mutation, so protected-root resolution meets an absent path first. A
  first run on a fresh state root therefore fails until the operator
  pre-creates the directory; the engine creates the `.publication` work
  directory itself at setup. #238's
  `freesided setup` must create the driver directory (comment recorded
  on #238).
- **The opt-in live suite is a CI-blind drift surface, now twice.**
  #244's public-default topology broke the mint harness (#272), and
  #322's gate capability broke the recovery-drain path (#332); neither
  could surface in CI because the suite is opt-in. Revisit when #237
  lands: a scheduled live-suite run on the operator machine would close
  the class rather than catching each instance one smoke at a time.
