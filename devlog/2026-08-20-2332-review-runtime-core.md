# Provider-neutral review-runtime core (#872)

Extract a provider-neutral review-runtime seam in package `ward` so the
Codex ReviewSource and the forthcoming Claude shadow ReviewSource (#865)
share one audited container topology instead of two ~3,000-line copies.
Behavior-preserving for Codex.

## Decisions

- **Chose a provider seam over injected values, keeping the exported
  API Codex-named and byte-stable, over a full symbol rename to neutral
  names.** The `Codex*` review symbols are consumed outside the unit's
  declared path `daemon/internal/ward` (`cmd/freesided`,
  `internal/wardstore`), so renaming them to neutral/unexported names
  would ripple out of scope and become a cross-component change. The
  plan's Sizing watch pre-authorizes scoping #872 to "config +
  collection + launch + credentialStrategy interface" and deferring the
  deeper de-Codexing rename. The substantive deliverable #865 needs is
  the seam, not the cosmetic rename; the rename is a follow-up.
  (Agent judgment under the plan's Sizing watch; owner fiat `Handle
  #872`.)

- **Chose two seam interfaces: `reviewProvider` (stateless value seam)
  and `credentialStrategy` + `reviewAuthLease` (stateful credential
  seam).** The value seam supplies the finding-source / result-provider
  labels, the content-addressed envelope version tags, the prompt
  protocol, and the review command. The credential seam supplies the
  four launch-time credential hook points the neutral skeleton invokes
  (launch admission, lease acquire, mid-launch admission re-check, start
  admission + release). Separating them keeps the risky credential
  abstraction minimal and the value substitutions trivially auditable.

- **Chose to wrap the existing audited Codex auth functions rather than
  re-abstract them.** `codexCredentialStrategy` is a thin adapter over
  the unchanged `checkCodexAuthReenrollment`, `acquireCodexReviewAuth`,
  `verifyCodexAuthLaunchAdmission`, `reserveCodexAuthStartAdmission`, and
  `releaseCodexReviewAuthLease`. No credential-isolation logic moved.
  The plan directs "wrap rather than force full abstraction, keep the
  seam to only what #865 (setup-token mount, no refresh) needs." The
  always-non-nil `reviewAuthLease` reproduces the prior nil-guard no-op
  semantics (api-key mode) exactly, so no launch-path control flow
  changed.

- **Kept the exported entry points Codex-defaulted.**
  `CodexReviewResultEvidence`, `CodexReviewConfigurationDigest`,
  `BuildCodexReviewAgentSpec`, and `validateCodexReviewAgentSpec` keep
  their signatures and delegate to provider-parameterized internal
  variants (`reviewResultEvidence`, `reviewConfigurationDigest`,
  `buildReviewAgentSpec`, `validateReviewAgentSpec`) with the Codex
  provider. External callers are unchanged. Provider is an unexported
  field on `CodexReviewLifecycle` and `CodexReviewSourceConfig`, defaulted
  by the constructors and nil-defaulted by a same-struct accessor so
  focused tests that build the struct directly keep the Codex provider.

## Rejected alternatives

- **Re-receiver the ~40 `*CodexReviewLifecycle` core methods onto a
  neutral `*reviewLifecycle` type.** This is the "genuine core type"
  extraction, but it forces the exported-API rename (external ripple)
  and huge mechanical churn across ~8,300 lines of tests for no
  behavior change. Deferred with the rename.

- **Route the endpoints seam (`AuthMode.providerEndpoints()`) through
  the provider in #872.** Endpoints derive from `CodexAuthMode`, an
  inherently Codex-specific type that #865 extends when it adds a Claude
  auth mode. Abstracting endpoints now, without the second auth mode in
  hand, would be premature. Left Codex-auth-mode-derived; #865 owns it.

- **Abstract the credential body / mount-target derivation
  (`codexReviewAgentAuthSnapshot`, snapshot volume seeding) in #872.**
  These are the auth.json two-file OAuth shape. The Claude setup-token
  body/mount is genuinely #865's work (it knows that shape); forcing the
  abstraction now would over-generalize against one concrete case. The
  spec build stays Codex-credential-specific; #865 extends it.

## Verification findings (behavior-preserving proof)

Trust-boundary discipline (credential-isolation + volume-lease +
RO-mount surface; returned-object-trust boundary). Old-vs-new fuzzed
equivalence harness (`review_equivalence_test.go`) reconstructs the
pre-#872 pure implementations from base commit
`6bf2c8b854d6470e77ae5b93e6113530e35c3d90` and compares
decision-for-decision.

- **Confirmed — collection normalization / strict-JSON decode
  equivalent.** `FuzzReviewNormalizeCollectionEquivalence` (~200 interesting
  inputs over ~200k execs) found no divergence across exit statuses,
  malformed / trailing / duplicate-key / missing-array outputs,
  out-of-domain severities, and whole-file / inverted / non-positive
  line ranges. The P0–P3 gate and the `StartLine < 1` rule are
  preserved.
- **Confirmed — launch-spec mount + command + env derivation
  equivalent.** `TestReviewBuildAgentSpecEquivalence` compares the full
  `ContainerSpec` and `CodexReviewJournalBinding` field-for-field across
  command-affecting input variants (incl. shell metacharacters,
  unicode, deep workspace target); `FuzzReviewCommandEquivalence`
  (~250k execs) proves the provider review command is byte-identical to
  base `codexReviewCommand`.
- **Confirmed — value seam matches base literals.**
  `TestReviewProviderConstantsMatchBase` pins each `codexReviewProvider`
  method to the exact base-commit literal (labels, version tags, prompt
  protocol, topology version).
- **Confirmed — no credential-isolation regression.** The credential
  path delegates to the unchanged audited functions; the existing Codex
  review suite (incl. the credential-separation, exclusive-lease, and
  RO-mount assertions) passes rename-adjusted only. No
  publication-credential reachability introduced.
- **Rejected-by-verification — none.** No divergence surfaced; nothing
  to re-raise.
- **Accepted-by-decision — the seam is exercised only by the Codex
  provider in this unit.** The provider indirection is proven transparent
  for Codex; its second consumer (a genuinely different provider) lands
  in #865.

## Revisit when

- #865 needs a launch decision this unit left Codex-hardcoded
  (endpoints, credential body/mount, binding topology-version legacy
  tolerance): promote that point to the provider seam then, in #865.
- The deferred de-Codexing rename is scheduled: it renames the ward
  review symbols to neutral names and updates `cmd/freesided` +
  `internal/wardstore`, so it is a cross-component (and API-surface)
  unit, not a lane-local edit.

Follow-up: #874 (the deferred de-Codexing rename).
