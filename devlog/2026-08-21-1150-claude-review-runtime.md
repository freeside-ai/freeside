# Claude shadow review runtime (#865)

Implement the thin Claude provider on the #872 provider-neutral
review-runtime core: a setup-token credential strategy with no host-side
refresh, the Claude CLI review command, the Anthropic provider labels, and
a distinct topology and evidence-version namespace. Reuses the audited
container topology unchanged (read-only workspace snapshot under an
exclusive volume lease, networkless read-only observer, provider-only
egress); copies none of the Codex topology. Behavior-preserving for Codex.

Builds on [[2026-08-20-2332-review-runtime-core]] (#872). Chain: #872 → #865
→ #845 → #846.

## Decisions

- **Threaded the instance provider through the launch/validation path;
  used an all-providers enumeration only for teardown/recovery.** Launch
  and reconstruction (`buildReviewAgentSpec`, `validateReviewAgentSpec`,
  `binding.validate` and its wrappers, `validateCodexReviewLaunch*`) take
  the lifecycle/source's own `provider` as authoritative, never a value
  decoded from persisted state. This keeps the trust boundary at
  reconstruction consistent with the project convention (a decoded trust
  bit is never trusted). `validatedResourceNames`, which authenticates
  *existing* resources for teardown/recovery, instead enumerates every
  known provider's current name-set (`allReviewProviders`) and lets the
  persisted intent's own container name select the match — so a Claude
  review's `-claude` container is reaped as readily as a Codex one without
  threading a provider into intent-scoped helpers. Rejected: resolving the
  provider from the binding's `TopologyVersion` inside `validate`, which
  would let attacker-influenced persisted state pick the validation
  profile.

- **The setup token never enters the container environment.** It is read
  from the read-only snapshot file into a shell variable inside the review
  command and passed to the CLI inline as `CLAUDE_CODE_OAUTH_TOKEN`,
  exactly as `exec/claude/spec.go` does. `claudeReviewProvider.containerEnv`
  carries only non-secret config (HOME, CLAUDE_CONFIG_DIR, the
  disable/sandbox flags), so the token is absent from `AgentSpec.Env`, the
  digested `LauncherEnvironmentDigest`, and the `CommandDigest` (the
  command template holds the `$token` variable reference and the snapshot
  *path*, never the value).

- **`requiresExpiringCredential()` gates the lifetime-floor requirements.**
  A setup token has no access-token expiry, so the positive-floor and
  refresh-threshold gates (request, launch-shape, and configuration
  envelope) apply only to expiring-credential providers; a Claude binding
  carries `AccessTokenExpiresAt == nil`.

- **Provider-aware outcome validation resolves from the persisted result
  label.** `CodexReviewSourceOutcome.Validate` now recomputes completion
  evidence with the provider resolved from `Result.Provider` via
  `reviewProviderByProviderLabel`, failing closed on an unknown label.
  This is the #875 handoff finding: a Codex-hardcoded recomputation would
  turn every successful Claude result (distinct `resultEvidenceVersion`)
  into a durable contradiction.

- **Kept the exported `Codex*` API byte-stable; reached Claude through new
  constructors.** `BuildCodexReviewSnapshotObserverSpec` /
  `ObserveCodexReviewSnapshot` delegate to internal provider-aware
  functions; the Claude runtime is reached via `NewClaudeReviewLifecycle`,
  `NewClaudeReviewSource`, and `ClaudeReviewConfigurationDigest`. The
  de-Codexing rename of the neutral symbols stays #874.

## Refute-first pass (credential-leak surface)

Lenses attempted to reach the token; all rejected-by-verification:

- **Token via the workspace mount** — rejected: token lives only on the
  separate read-only snapshot volume; the command reads it into a shell
  var, never writes it to the read-only workspace.
- **Token via `AgentSpec.Env` / launcher digest** — rejected:
  `containerEnv` carries no token; `TestClaudeReviewBuildAgentSpecTopology`
  asserts no env entry contains the token or a `CLAUDE_CODE_OAUTH_TOKEN=`
  prefix.
- **Token via the command/binding digests** — rejected: the command
  template references `$token` and the snapshot path, not the value, so
  `CommandDigest` binds no secret; the binding stores only
  `AuthSnapshotDigest` (a digest).
- **Cross-provider auth mode** — rejected: a Codex subscription request
  against the Claude provider and a Claude setup-token request against the
  Codex provider both fail at admission
  (`TestClaudeReviewRejectsCrossProviderAuthMode`); `inspectCodexHostAuth`
  rejects `setup_token` fail-closed.
- **V2 legacy topology on a Claude binding** — rejected: the Claude
  provider lists no legacy topology versions, so even the teardown path
  refuses a v2-stamped Claude binding
  (`TestClaudeReviewBindingRefusesV2LegacyTopology`).

Accepted-by-decision: the token transits the same host-staged 0400 file
under the private 0700 `ExportRoot` stage that the Codex auth.json already
uses, wiped on return and reaped on crash by `recoverCodexReviewIntent` —
unchanged audited path.

## Surfaced for the spine (not in this unit)

- **Daemon composition wiring** for the Claude shadow review
  (`cmd/freesided` flags, preflight, config digest) is undeclared by #865,
  #845, and #846. Out of this unit's declared scope (`daemon/internal/ward`
  only); flagged as a chain gap.
- **`--json-schema` structured-output envelope**: the command extracts
  findings via `jq -e .structured_output`. The exact envelope field is an
  assumption confirmed against CLI help at build time; the live suite is
  the runtime confirmation. If a run shows the field is named differently
  or the schema lands under `result`, widen `claudeReviewCommand`'s
  extraction and the collection fixtures.

## Codex review findings (PR #882)

Root cause of the recurring class: the unit's tests exercised the leaf
functions (`buildReviewAgentSpec`, `normalizeCollection`, materialize)
directly but never drove a Claude-configured source/lifecycle through the full
`RequestReview` → launch → cleanup path, so Codex-hardcoded values in that
integration path passed CI. After the third same-class member, a fresh-context
adversarial refute pass swept the whole path for remaining provider-hardcoded
values; it found one (the control-path gate below) and confirmed nothing else.

Round 1 (both P1, confirmed, folded):

- **Lifecycle used the Codex container name for a Claude launch.** The spec
  build created `-claude`, but `codexReview` journalled the intent, its
  resource, and the launch-failure cleanup target with the `-codex` default,
  so a Claude launch would `ErrParentKeyMismatch` on the mark and leave the
  credential-bearing container behind on cleanup. Fixed: all three derive
  from `reviewContainerName(b.reviewProvider(), runID)`, the single source of
  truth the spec uses. Regression test
  `TestClaudeReviewContainerNameIsSingleSourced`.
- **Outcome validation let the decoded provider label select its validator.**
  Under the rewritten-row model a flipped label plus recomputed (unkeyed)
  evidence would self-validate. The exported `Validate` is called by the
  provider-agnostic persistence layer (`wardstore`) and cannot take a
  `reviewProvider`, so the provider-namespaced recomputation moved out of it:
  `Validate` is now shape-only, and `verifyCompletionEvidence(provider)`
  re-gates at the domain layer (source/lifecycle) where the trusted provider
  is known — the persisted label must equal the trusted provider's label
  (fail-closed) and the evidence must then recompute. This keeps the #875 fix
  without trusting the decoded label. Point-of-use gate at every outcome-load
  site; corruption detection preserved there.

Round 2 (P1, confirmed, folded):

- **Instruction materialization hardcoded the Codex vendor.** The source's
  `materializeReviewInstructions` built `VendorInstructions` with
  `AgentVendorCodex`, so a Claude source's every `RequestReview` was rejected
  at the launch-shape vendor gate. Fixed: `s.reviewProvider().vendor()`.
  Regression test `TestClaudeReviewSourceMaterializesClaudeVendor` drives the
  source path the topology tests bypassed.

Round 2 self-found (refute pass; non-blocking, fixed in the same push):

- **Control-path overlap gate protected the Codex home/config targets.**
  `codexReviewWorkspaceOverlapsControlPath` hardcoded the Codex container
  control dirs, so for a Claude review it protected the wrong paths: a
  `WorkspaceTarget` overlapping Claude's writable home/config was admitted, and
  a target overlapping a Codex path (harmless for Claude) was spuriously
  rejected. Deployment-owned config and fails closed (a shadowed writable dir
  aborts the container under `set -e`), so defense-in-depth, not exploitable.
  Fixed: the gate derives the protected home/config from
  `provider.homeTarget()`/`configHomeTarget()` (behavior-preserving for Codex;
  the auth/instruction file entries were subsumed by the home dir). All four
  call sites threaded; the exported observer builders keep their signatures via
  provider-aware internal delegates.

Round 3 (P1, confirmed, folded):

- **Claude review invocation omitted `--safe-mode`.** Without it the CLI loads
  candidate-controlled customization (the reviewed repo's `.claude` settings,
  commands, hooks, MCP config); combined with `--dangerously-skip-permissions`
  a reviewed repo could influence its own shadow review and suppress or
  fabricate findings, against the plan's launch contract (docs/plan.md
  §1258-1264) and the proven exec Claude launcher. Fixed: added `--safe-mode`
  to `claudeReviewCommand` (only the admitted instruction bundle via
  `--append-system-prompt-file` and administrator policy stay authoritative);
  command-shape test updated. A distinct class from rounds 1–2 (a missing
  hardening flag, not a Codex-hardcoded value).

Round 4 (P2, confirmed, folded):

- **The source constructors accepted a lifecycle from the other provider.**
  A Claude source configured with a Codex lifecycle (or the inverse) passed
  construction because the shared constructor checked only that the lifecycle
  was initialized. Every request then failed at the lifecycle's trusted
  provider-specific vendor/auth gates. Fixed: the shared constructor now
  requires the lifecycle's provider label to match the source provider before
  accepting the configuration. `TestReviewSourceRejectsLifecycleProviderMismatch`
  covers both mismatch directions and the valid Claude pairing. A distinct
  constructor-coherence class, not a recurrence of the request-path hardcoding
  class from rounds 1–2.

Round 5 (P1, confirmed, folded):

- **Restart recovery ran every shared-journal intent through its one lifecycle
  provider.** A Codex-composed recovery encountering a started Claude review
  validated the binding as Codex, refused the Claude topology, and left the
  credential-bearing container and snapshot unreconciled. Fixed: recovery now
  re-derives exactly one provider by validating the complete binding and the
  independently journalled coherent intent topology against the closed trusted
  provider registry, then passes that provider explicitly through authenticated
  abort cleanup. It never trusts a stored provider bit or the container suffix
  alone. `TestCodexReviewRecoveryDispatchesStartedClaudeIntent` drives the
  exported Claude source through launch, simulates restart under the existing
  Codex-composed recovery, and proves the intent closes, outcome becomes ready,
  and Claude container is gone.

Five-round checkpoint: **go**. Each prior fix held, rounds shrank to one
independent reachable blocker, CI stayed green, and this round closes the only
shared-journal recovery path rather than reopening an earlier class.

Round 6 (P2, confirmed, folded):

- **Terminal failure parsing recognized only the Codex event envelope.** A
  reachable nonzero Claude `type: "result", is_error: true` response hid its
  `result` message from quota/configuration classification, so quota retried
  instead of escalating and invalid authentication retried instead of parking.
  Fixed: the provider seam now extracts only its own structured terminal-error
  envelope before the shared message classifier runs. Claude regression cases
  cover quota, authentication, and rejection of a non-error envelope as
  classification authority; the unchanged Codex classifier matrix remains
  green.

## Revisit when

`review_provider.go` interfaces change, #874 renames the neutral symbols,
`exec.ReviewSource`/`exec.ReviewResult` change, the agent-claude image pin
or `CredentialManifestSetupToken` semantics change, or the live suite shows
the `--json-schema` envelope differs from `.structured_output`.
