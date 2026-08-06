# Implementer workspace hydration in the launch wrapper (#522)

## Decision

Chose to hydrate the unattended implementer's workspace **inside the Claude
launch command's existing root phase** (`daemon/internal/exec/claude/spec.go`
`agentCommand`), running the project image's `PreparationCommand` before the
ownership sweep, with the argv resolved at composition from the immutable
`project_images` record. Rejected adding pre/post one-shot containers to the
ward handoff and baking `node_modules` into the image.

The launch command already runs a root phase that prepares the workspace
(chown, evidence dirs) before `setpriv` drops to the agent user. Hydrating
there means the same already-attested writer does the work: no new writer
lifecycle, no new `waitStopped`/teardown proofs, and no ordering constraint
against `observeSeededBase` (the agent container starts only after the
seeded-base attestation, so hydration cannot touch it by construction).

## Rejected alternatives

- **Pre/post one-shot containers in the ward handoff.** Each extra container is
  another writer inside §5.7's one-writer proof chain
  (`daemon/internal/ward/handoff.go`): ownership labels, termination proofs,
  teardown arms, plus an ordering constraint against `observeSeededBase`.
  Substantial risk in a refute-first file for behavior the wrapper already
  provides.
- **Baking `node_modules` into the image.** Loses the proven offline-hydration
  shape (`projectimage/builder.go` `proveOffline` runs prepare + each recipe
  argv under `--network none`) and bloats the image for stale trees.
- **Widening `StartSpec`/`ExecutionAdmission` with the preparation command.** A
  `kind:contract` unit, avoided: the *shared* admission contract need not carry
  it, because the immutable `project_images` record is the authority production
  publication itself joins image → preparation command against
  (`engine/production_publication.go` `loadBinding`). The argv is instead
  persisted in the driver's own package-private `intent` record (state.go), not
  in the shared contract, which is what recovery needs (see Recovery durability
  below).

## Trust boundary and refutation

`image.PreparationCommand` is a deserialized store field injected into the root
wrapper's argv. It is gated, not trusted: composition (unattended only) refuses
at startup unless the record's Repository/RepositoryID/CommitSHA match the
configured repo/base **and** the decoded command is exactly the fixed
image-owned helper `[]string{projectimage.PreparationPath}`
(`resolveProjectImagePreparation`), each argv element is shell-quoted
(`shellJoin` → `shellQuote`), and the domain layer already rejects NUL bytes and
an empty command (`domain/project_image.go` `Validate`). A mismatched base would
otherwise let the prepare helper's manifest guard exit 42 mid-run; catching it
at daemon start closes the run-482 window (a spent implementation before
publication binding rejected the same mismatch).

The command re-gate is the reconstruction trust boundary (AGENTS.md daemon
conventions "Trust boundaries at reconstruction/persistence"), added in response
to Codex review (PR #528). `Validate` bounds only empty/NUL and shell-quoting
bounds only injection, so neither constrains *which* command runs: a corrupted
or tampered but self-consistent row (matching identity, arbitrary argv) would
otherwise reach `agentCommand` and execute as root beside the credential mount
and writable workspace. Onboarding already gates the same field to the identical
fixed value (`operations/onboard.go`); this boundary re-runs that gate and fails
closed rather than trusting the decoded argv.

The same re-gate is applied one layer down at the exported driver constructor
`claude.New` (PR #528, second Codex P1): the `Preparation` field of the exported
`claude.Config` reaches the root argv, so any present or future caller (the note
already flags #407 reusing this plumbing under "Revisit when") that builds a
config directly is a bypassable trust boundary. `New` now accepts only an empty
command or `[]string{projectimage.PreparationPath}`, so the invariant holds for
every caller rather than only the composition path. This is the class widened to
its full boundary set (composition, onboarding, constructor), not a patch of the
single cited line.

Preparation-failure diagnostics (PR #528, Codex P2): a nonzero prepare exit
surfaces as the pre-agent sentinel 87, which recovery previously reported with
the bare `Claude writer exited with status 87` line, indistinguishable from an
agent failure. `pipeline.go` now maps that sentinel to a named preparation-fault
summary so the operator triages the environment fault (the hydration helper
exited nonzero, e.g. its manifest guard exit 42) apart from an agent exit.
Capturing the helper's bounded stderr into protected evidence for the *cause*
(the heavier of Codex's two offered options) is deferred to #529.

## Recovery durability (Codex round-3 P1, confirmed-and-fixed)

The preparation argv is now persisted in the driver's package-private `intent`
record (`state.go`) and recovery rebuilds the launch command from `in.Preparation`
(`spec.go handoffSpec`), never from the live `d.prepare`. This corrects a real
newly-introduced defect and a wrong assumption in the original rationale above.

The original text claimed composition "re-derives it deterministically on
recovery, exactly like `cfg.AgentImage`." That was false: the recovered image is
sourced from the *persisted* `spec.ImageRef`, and prompt/seed/base/instructions
from the persisted intent, so before this fix `prepare` was the **only**
command input taken from mutable composition state. Ward binds `Agent.Command`
into `SpecDigest` (`ward/journal.go`, `ward/recover.go`), so a recovery that
rebuilt the command from a changed `d.prepare` produced a digest mismatch that
`Recover` rejects and the driver retries indefinitely (`pipeline.go`
`ErrRecoveryRetryable`), stranding the run and its credential lease. Two live
triggers: an in-flight unattended run recovered after this version deploys (its
journal carries the pre-hydration command), and an unattended run restarted
under attended composition.

Fix and its class sweep (one push): `Preparation []string json:"preparation,omitempty"`
on `intent`, captured at `StartWithInputs` from `d.prepare`; `handoffSpec` reads
`in.Preparation`. Backward-compat is structural: an old record has no
`preparation` key, decodes to nil, and reproduces its original no-prepare
command, matching the journalled digest (`omitempty` also keeps attended records
byte-identical). `intent.validate()` re-gates the decoded argv to empty-or-fixed-helper
(the decode is a reconstruction trust boundary, joining the constructor,
composition, and onboarding gates). Command-input audit: image←`spec.ImageRef`,
prompt←`in.Prompt`, prepare←`in.Preparation`, all persisted; no command input
now comes from mutable `Driver`/config state.

- **Rejected-by-verification (from the original filing): "verifier version
  drift."** gh-imgup's `package-lock.json` at the run base (`9595682e`) pins
  `@biomejs/biome` 2.5.6, satisfying `^2.5.1`; the verifier's
  `npm ci --ignore-scripts` ran exactly that, and repo CI resolves the same
  lockfile. The schema-mismatch notice was gh-imgup's own stale `biome.json`
  `$schema` URL, target-repo hygiene, not a Freeside defect. Verifier tool
  resolution needs no work.

## Scope note

Declared scope widened by one read-only store method,
`GetProjectImageByRef` (`daemon/internal/store/project_image.go`): the image
reference is globally unique, so binding a configured image back to its
provenance needs a by-ref read, and the per-repository `ListProjectImages`
scan could not name a repository-ID mismatch as its own refusal arm.

## Revisit when

The Codex implement driver (#407) lands and composes its own wrapper (it reuses
this composition plumbing), or `PreparationCommand` stops being a single fixed
image-owned helper (per-recipe or agent-mutated preparation would break the
exit-42 manifest guard for the implementer as it already does for the
verifier).
