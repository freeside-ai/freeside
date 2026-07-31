---
run: manual
stage: wave2-adversarial-review
date: 2026-07-31
branch: docs/wave2-adversarial-review
---

# Review Phase 1A Wave 2 Adversarially

Audited commit:
`2a1a90dc7b5d9dc875b4b43c4b3545c3132a4064` (`origin/main` after a fresh
fetch).

**Verdict: Phase 1A Wave 2 does not satisfy its claimed exit.** Three
findings survived the specification, decision-history, issue-queue, code-trace,
and executable-reproduction gates. The P1 is structural: real Claude work
stops after import and `ExecutionExport`, before clean verification,
execution-bound publication, PR creation, and the ready/mobile handoff. Two
P2s expose credential authority beyond its approved boundary: the App manifest
conversion code rides process arguments, and returned installation-token
expiries have no upper bound.

Filed findings: #411, #412, and #413.

## Method

This was the fresh-context Phase 1A exit review required by `docs/plan.md`
Section 11. The repository and its checked-in documents were the only design
authority. Every candidate was first compared with the exact plan claim,
`docs/history/decisions.md`, relevant decision notes, and the complete open
issue queue. An accepted owner decision or existing issue removed the
candidate from the filing path.

Every surviving candidate received three independent checks:

1. A code trace from externally reachable composition to the claimed gate or
   effect.
2. A specification and precedent check against the plan, history, prior
   adversarial review, and work-unit acceptance.
3. An executable reproduction in a scratch archive of the audited commit
   under `/tmp`; no reproduction changed the repository.

The review classified severity by concrete failure, not by the amount of code
needed to fix it. P1 means the Phase exit or a load-bearing safety path is
false. P2 means a real credential or trust-boundary failure exists but does
not itself create an immediate unauthenticated publication path. P3 means a
lesser in-scope defect worth recording. No P3 survived.

## Hunt Categories

- Credential leakage through argv, environment, logs, errors, transcripts,
  exports, and container filesystems.
- Ward egress enforcement, writer/exporter separation, mount identity,
  seeded-base identity, read-only guarantees, teardown, and recovery.
- Reconstruction boundaries for admission, trust, publication, export,
  backup, installation, and returned API objects.
- Publication reachability and the workflow-audit, trust-profile,
  reservation, source-head, gauntlet, recipe, and protected-path gates.
- Admission capability floors, operating-mode honesty, waiver retirement,
  conformance freshness, backup health, and blocking system health.
- API, daemon, app, operator-command, and exit-claim drift.
- Setup, onboarding, installation intent, janitor, keystore, and trust
  activation authority, including replay and interruption windows.

## Confirmed

### P1: Real Runs Stop Before Verification and Publication

Filed as #411.

The Phase 1A.2 flow is `Claude → proven credential mode → proven ward handoff
→ gauntlet → clean verifier → audited publication → iPhone`
(`docs/plan.md:1782-1788`). The gauntlet contract puts the trusted recipe in a
credential-free, networkless workspace before the git/publish service
(`docs/plan.md:511-525`). Closed work item #237 used the same full-chain
objective.

Two earlier work units deliberately stopped short of that composition. The
execution-head binding note rejected a dead engine helper until its clean
verification, authorization, reservation, and transport inputs existed
(`devlog/2026-07-30-0949-execution-export-head-binding.md:67-72`), and the
Claude-driver note corrected its live harness to claim only the
pre-publication boundary
(`devlog/2026-07-28-1845-claude-driver.md:425-428`). This finding does not
overturn those work-unit scope decisions. The changed condition is the
integrated Wave exit: Section 11 now judges the merged system against the
complete 1A.2 flow, so a valid earlier deferral is an unmet exit criterion
when the composition it deferred still has not landed.

The shipped live path says its scope ends at the pre-publication
`ExecutionExport` (`scripts/run-real-work.sh:9-11`;
`daemon/internal/integration/production_run_live_test.go:25,113`). The Claude
pipeline imports the handoff and returns a completed stage result
(`daemon/internal/exec/claude/pipeline.go:45-79,184`). The engine accepts that
result into `production_stage_terminal`
(`daemon/internal/engine/production_workflow.go:616-717`). Production
composition constructs neither a verifier nor a publisher
(`daemon/cmd/freesided/main.go:440-487`;
`daemon/cmd/freesided/claude_driver.go:578-691`). The only non-test
`verify.Verify` call belongs to the attended fake-publication workflow, and
`PublishExecutionAfterGateAndFinalize` has no non-test caller.

A scratch-only integration test completed the existing production fixture and
ran three reconcile passes. The engine accepted head `cafe1234` while both
publication reservation and publication intent queues remained empty:

`GOCACHE=/tmp/freeside-wave2-go-cache go test ./internal/integration -run
TestWave2AdversarialReproCompletedProductionRunStopsBeforePublication -count=1
-v`

The result was:
`CONFIRMED: accepted head cafe1234 produced no reservation and no publication
intent`.

Concrete failure: the only real unattended workflow can be reported completed
without the clean verifier, publication safety gates, GitHub PR, or
ready/mobile result that define the Phase 1A.2 exit.

### P2: Setup Exposes the Manifest Code in Process Arguments

Filed as #412.

The registrar correctly treats the one-time manifest conversion code as
credential-equivalent until GitHub consumes it and redacts code-bearing URLs
(`daemon/internal/publish/register.go:230-275`;
`devlog/2026-07-16-1223-publish-credential-containment.md:157-174`). The
packaged command nevertheless accepts it as `-registration-code`
(`daemon/cmd/freesided/setup.go:48-52,145-154`), and the operator README
instructs that literal form (`daemon/README.md:69-76`).

The scratch reproduction built the audited `freesided`, launched setup with an
inert marker, stopped it after `exec`, and inspected the live process:

`ps -ww -o command= -p <pid>`

The output contained:
`-registration-code FREESIDE_REPRO_MANIFEST_CODE_NOT_A_SECRET`.

Concrete failure: the credential-equivalent code is visible in the process
table and commonly persists in shell history. A local observer that wins the
one-time exchange receives the App private key.

### P2: Installation-Token Expiry Is Not Upper-Bounded

Filed as #413.

The mint response is untrusted until Freeside verifies the exact repository,
permissions, and expected bounded expiry (`docs/plan.md:492-506`;
`devlog/2026-07-22-2124-multi-account-agent-identity.md:22-33,193-200`).
`mintResolved` instead accepts every parsed expiry after `now`
(`daemon/internal/publish/mint.go:281-310`), audits and returns it
(`:315-336`), and both token caches trust it until two minutes before the
response-supplied time (`daemon/internal/publish/tokens.go:57-85,150-168`).
Authenticated git then receives the token through its child-only environment
(`daemon/internal/publish/gitnet.go:296-319,345-355`).

A scratch-only test served an otherwise exact response with
`expires_at: 2126-07-16T13:00:00Z`:

`GOCACHE=/tmp/freeside-wave2-go-cache go test ./internal/publish -run
TestWave2AdversarialReproMintAcceptsUnboundedExpiry -count=1 -v`

The result was:
`CONFIRMED: accepted and audited token expiry 2126-07-16 13:00:00 +0000 UTC,
876577h0m0s after mint`.

The class also reaches the janitor's full-grant token. Its response omits
expiry entirely (`daemon/internal/publish/janitor.go:834-840,924-940`), while
revoke-failure handling assumes the credential lasts only one hour
(`:861-869`).

Concrete failure: a forged or regressed response turns nominally short-lived
GitHub authority into a token Freeside accepts, audits, caches, and hands to
git for an arbitrary lifetime.

## Rejected by Verification

1. **Provider-only egress is enforced, not merely attested.** Runtime creation
   uses an internal Apple container network; ward verifies the exact
   host-only network and re-inspects before start
   (`daemon/internal/ward/runtime_cli.go:138-145`;
   `daemon/internal/ward/handoff.go:617-690`). The CONNECT proxy admits only
   exact allowed authorities with matching TLS SNI
   (`daemon/internal/ward/egress.go:31-107,165-271,331-427`). Full
   conformance positively reaches the provider and negatively checks
   undeclared CONNECT, direct IP, and guest DNS
   (`daemon/internal/ward/suite.go:193-245`). This matches
   `docs/plan.md:392-399`.
2. **The exporter does not share the credential-bearing VM.** The live path
   observes the writer stopped, deletes it, proves absence, and closes the
   proxy before a fresh exporter is independently created and inspected
   (`daemon/internal/ward/handoff.go:692-713,910-982`). Conformance proves
   the credential remains only on the omitted volume and checks networkless,
   read-only export (`daemon/internal/ward/suite.go:990-1044`).
3. **The Claude setup token is absent from the recorded spec environment.**
   The only production handoff builder returns a nil agent environment and
   reads the mounted token inside fixed launcher text at exec
   (`daemon/internal/exec/claude/spec.go:47-51,96-139,232-252`). Runtime
   create errors with credential-capable argv or environment do not retain
   stderr (`daemon/internal/ward/runtime_cli.go:62-76,104-126,231-236`).
4. **Known Anthropic tokens do not evade scanning at chunk boundaries.** The
   scanner reads every regular file in overlapping chunks and matches the
   supported `sk-ant-` token classes without rendering matched bytes
   (`daemon/cmd/freesided/credential_scan.go:25-56,61-110`). The supported
   format and future-format revisit are explicit in
   `devlog/2026-07-30-0952-anthropic-secret-rule.md:1-26`.
5. **Ward recovery does not trust ordinary decoded lease, base, or export
   facts.** It validates the supplied spec and digest, binds the persisted
   lease to that spec, re-resolves the current auth-store volume, checks the
   observed base, constrains materialized paths to the daemon-owned shape,
   and re-runs export verification and scanning
   (`daemon/internal/ward/recover.go:130-184,220-244,727-735`;
   `daemon/internal/ward/export_verify.go:157-195`).
6. **A zero outcome marker does not synthesize recovery success after proxy
   proof is lost.** Live success requires proxy-health-throughout before
   `WriterComplete`; recovery adopts only an already durable completion bit
   and revalidates marker and absence. This preserves
   `docs/plan.md:664-699`.
7. **The auth-store lease covers the ordinary bounded handoff.** Its requested
   expiry includes handoff timeout, teardown timeout, and margin; ward checks
   it again immediately before start and releases only after teardown
   (`daemon/internal/ward/handoff.go:679-687,1128-1161,1185-1217`).
   Permanently unreconcilable journals remain tracked by #385.
8. **Git installation tokens do not appear in git argv, stored config, or
   surfaced command output.** The transport replaces the environment,
   disables helpers and redirects, injects the token only into a fresh child
   environment, and reports tokenless args and refusal classes
   (`daemon/internal/publish/gitnet.go:114-150,292-355`). `Secret` redacts
   implicit formatting (`daemon/internal/publish/secret.go:8-50`).
9. **The ward host gateway does not make the production daemon listener
   reachable.** Production rejects wildcard and arbitrary binds, permitting
   only loopback or an exact local Tailscale address
   (`daemon/cmd/freesided/main.go:686-733`). The live writer negative probe is
   recorded in
   `devlog/2026-07-27-1531-host-listener-isolation.md:39-48`.
10. **Ordinary candidate automation and reviewer-instruction paths are
    publish-blocking in the implemented fake path.** Importer mandatory path
    classes and the verifier's candidate-control flags are applied before the
    fake publisher gate (`daemon/internal/importer/policy.go:15-68,151-177`;
    `daemon/internal/engine/fake_publication.go:1640-1696`). The real path's
    absence is the broader confirmed #411, not a second bypass finding.
11. **Decoded execution and publication trust bits are reconstructed.** Store
    reads use the current approved-recipe and admission policy gates, while
    execution-bound publication authenticates immutable admission, reservation,
    base, repository, and export-head joins
    (`daemon/internal/store/entities.go`;
    `daemon/internal/publish/execution.go:12-111`). The remaining
    accept-transaction race is already #316.
12. **Encrypted local checkpoint tampering does not produce a healthy
    restore.** Wrong keys, ciphertext changes, forged plaintext digests,
    symlinked or widened key files, and incomplete artifact closure are
    rejected by the encrypted-checkpoint tests and implementation
    (`daemon/internal/store/encrypted_checkpoint.go`;
    `daemon/internal/store/encrypted_checkpoint_test.go`). Portable key wraps
    remain #266, so local key colocation is not re-raised.
13. **Known recovery and durability candidates are already tracked.** Do not
    duplicate unreconcilable journal recovery (#385), missing durable
    auth-store mutation observation (#377), export residue (#387),
    materialized-export crash recovery (#371), symlink-bearing base seeding
    (#339), accepting-transaction re-gating (#316), or per-run store-policy
    resolution (#313).
14. **Production AgentClaims presentation is already tracked.** #381 owns
    persistence on a durable review surface. It remains relevant to the
    eventual #411 ready/mobile result but is not re-filed.
15. **Packaging's external clean-machine time targets remain an explicit
    Wave-exit gap, not new evidence.** PR #410 recorded the live GitHub,
    Apple-container, and timing checks as not run, and #231 remains the open
    Wave 2 exit tracker. No duplicate issue was filed from an unexecuted
    candidate.

## Accepted by Decision

1. **The setup token is ambient in the contained writer process tree.** The
   agent can read its mounted file and children inherit the launcher
   environment. Provider-only egress and export scanning are the accepted
   backstops (`docs/plan.md:374-385`;
   `devlog/2026-07-29-1750-claude-credential-topology.md:63-68`;
   `docs/history/decisions.md:628-635`).
2. **Secret scanning is best effort.** Encoded, split, or unsupported opaque
   formats can evade it by explicit posture (`docs/plan.md:387-390`;
   `daemon/cmd/freesided/credential_scan.go:19-23`;
   `devlog/2026-07-30-0952-anthropic-secret-rule.md:24-26`).
3. **An operator can persist a secret placed in the work specification or
   prompt.** The rendered prompt is durable driver input; this operator-input
   posture is distinct from setup-token delivery
   (`devlog/2026-07-28-1845-claude-driver.md:693-696`).
4. **The host gateway remains a network neighbor.** Other host services own
   their binding policy; Freeside separately proves its production listener
   unreachable (`docs/plan.md:392-399`;
   `devlog/2026-07-27-1531-host-listener-isolation.md:6-20,59-63`).
5. **A matching durable conformance pass may be restored on restart.** Startup
   does not unconditionally rerun the full suite; the scheduled doctor reruns
   it before evaluating health. This is explicit owner posture
   (`devlog/2026-07-28-1845-claude-driver.md:170-174`;
   `devlog/2026-07-27-2247-durable-backend-conformance.md:78-84`;
   `devlog/2026-07-30-2350-operational-command-packaging.md:186-203`).
6. **Phase 1A setup does not install the binary or supervisor itself.** It
   creates the owner-private state and App authority; the static binary and
   narrow elevation step remain manual
   (`devlog/2026-07-30-2350-operational-command-packaging.md:7-19`;
   `daemon/README.md:81-84`).
7. **Standalone doctor reads durable conformance while scheduled doctor
   refreshes it.** The schedule, not every one-shot diagnostic invocation,
   owns the full-suite rerun
   (`devlog/2026-07-30-2350-operational-command-packaging.md:136-151,186-203`).
8. **The Phase 1A backup key is local beside the encrypted checkpoint.**
   Portable wrapping, replication, and recovery authority remain the explicit
   #266 follow-up
   (`devlog/2026-07-30-1620-encrypted-backup-checkpoint.md:21-31`).
9. **The process-local verifier room is an attended fake only.** It openly
   cannot deny network or filesystem access and must not satisfy unattended
   isolation (`daemon/internal/verify/procroom.go:28-43`). The missing real
   networkless room is part of #411, not a claim that `ProcRoom` is stronger
   than documented.

## Verification

- Passed: a fresh fetch bound the review to audited `origin/main` commit
  `2a1a90dc7b5d9dc875b4b43c4b3545c3132a4064`.
- Passed: the scratch production-pipeline reproduction accepted a head without
  creating a publication reservation or intent.
- Passed: the scratch process-list reproduction exposed an inert manifest-code
  marker in the live setup command.
- Passed: the scratch mint reproduction accepted and audited a 2126
  installation-token expiry.
- Checked: the cited plan claims, decision history, decision notes, issue
  dispositions, command composition, and code traces support the confirmed,
  rejected, and accepted classifications above.
- Not run: live Apple-container conformance, a real GitHub token mint, and
  external clean-machine setup/onboard timing and callback flows. None was
  needed to demonstrate the three confirmed failures.

## Revisit When

Re-run this exit review from a freshly fetched base after #411, #412, and #413
are resolved and the spine has revalidated the Wave 2 exit evidence. The
follow-up must treat a new base commit as invalidating this review's CI,
full-diff, and integration evidence.
