# Fuzz the Full Review Launch-Spec Equivalence Surface (#968)

Complete #872's surface-(b) old-vs-new proof: fuzz the whole review
launch-spec and journal-binding decision surface (mount, environment,
command, endpoint, credential-binding, boundary), not just collection
normalization and the provider command, against the base-commit
reconstruction. Closes a Wave 6 adversarial-exit-review finding that the
credential-isolation / mount / environment trust boundary lacked the
fuzzed behavior-preservation evidence #872's acceptance required.
Test-only; no old-vs-new divergence was found (none was expected).

## Decisions

- **Chose to fuzz the whole builder through the existing fixture over
  extracting a pure production assembly seam.** A fully pure fuzz of only
  the assembly block would need a new production `assembleReviewSpec(...)`
  seam plus a matching old reconstruction: a production refactor beyond
  this test-only unit's declared scope. The existing
  `oldBuildReviewAgentSpec` already reconstructs the entire pre-#872
  path, so driving both it and production `BuildCodexReviewAgentSpec`
  over a fuzzed corpus proves the same decision-for-decision equivalence
  without touching production. The pure seam remains a possible follow-up
  rescope, not an in-unit change. (Agent judgment under the issue's
  declared scope; owner plan in #968 comments.)

- **Chose per-mode credential fixtures over per-iteration byte mutation
  for the I/O-gated `auth.json` / `AGENTS.md` surfaces.** These bytes are
  re-read under a private-dir / owner / symlink gate; mutating them every
  iteration would force file rewrites and snapshot-observation
  regeneration for no added seam signal, since both sides read identical
  bytes. The instruction (`AGENTS.md`) bytes stay at the fixture value;
  the credential (`auth.json`) bytes are fixed *per auth mode* -- the
  subscription case keeps the token fixture and the API-key success case
  uses a distinct valid API-key body with its regenerated snapshot
  observation -- so both supported modes reach a full build. Every binding
  field *derived* from these bytes (`AuthSnapshotDigest`,
  `AccessTokenExpiresAt`, `InstructionDigest`, `HostInstructionDigest`,
  `RepositoryInstructionSources`, ...) is compared each iteration, so the
  credential and instruction surface is proven, not silently omitted
  (issue acceptance #3).

- **Chose coordinated observation regeneration over hand-mutated opaque
  fields for the coupled proxy and .agents-presence levers.** The
  environment surface varies only through `cfg.ProxyURL`, which
  validation binds to the network observation's proxy authority; the
  shadow-mount count varies with the workspace observation's `.agents`
  entry. Both are regenerated through the real observer helpers
  (`testCodexReviewNetwork`, `testCodexReviewWorkspaceWithAgents`) so
  every fuzzed input is one the observer path could actually emit, and
  the environment/mount surfaces vary on the success path instead of only
  driving errors.

## Verification

- Bounded fuzz `FuzzReviewBuildAgentSpecEquivalence`: 63,050 execs over
  60s, zero divergences, no failing corpus written. The 19-case seed
  corpus (retained shell-metachar / unicode / deep-workspace /
  empty-value seeds, the API-key success seed, plus invalid and boundary
  inputs) runs under default `go test`.
- Discarded diagnostics confirmed the corpus is not a tautological
  all-error set: the success seeds reach full assembly with genuinely
  varying specs (baseline 6 mounts; deep workspace 7; `.agents`-absent 5;
  an alternate proxy changes the env slice and the launcher-environment
  digest; the API-key credential body yields nil access-token expiry, the
  `api.openai.com:443` endpoint binding, and a digest distinct from
  subscription), and the error seeds produce identical old/new errors.
- Scope of the guard: old and new can differ only in the two separately
  maintained assembly blocks, because the provider-seam methods and the
  final validator are shared calls -- the correct risk surface for a #872
  behavior-preserving refactor guard. The boundary field stays pinned to
  fresh-start (the only launchable boundary by design). The endpoint and
  credential-binding fields were pinned to subscription in the first draft;
  both a fresh-context adversarial review and Codex flagged that gap, so the
  API-key success path (distinct credential fixture, nil expiry,
  `api.openai.com` endpoint) was added, and those axes now vary across two
  supported modes alongside the command, environment, and mount assembly.

Revisit when: `BuildCodexReviewAgentSpec` / `buildReviewAgentSpec` gains
a decision input the fuzz does not vary, the `ContainerSpec` /
`CodexReviewJournalBinding` field sets change, or #872's evidence
contract is otherwise altered.
