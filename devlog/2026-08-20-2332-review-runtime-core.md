# Provider-neutral review-runtime core (#872)

## Decision

Chose a **provider-neutral review-runtime core** (extract the vendor-neutral
container topology into a shared core in package `ward`, migrate the Codex
review lifecycle onto it, then build the Claude runtime as a thin provider)
over **parallel Claude-named copies** and over **one large Claude runtime PR**.
Decider: owner (Ben), 2026-08-20, at the two decision gates during the "Handle
845" session.

## How the unit list got here

- `#845` (Claude shadow ReviewSource) planning sized it several multiples over
  the 1,000-line budget: no vendor-neutral base exists, so a faithful Claude
  source mirrors ~6,700 impl + ~8,200 test lines of Codex-named,
  Codex-specific code. Split (owner-approved) into `#865` (runtime, 845-A) and
  `#845` (state machine, 845-B, `starts-after #865`).
- Even `#865` alone, faithfully mirrored, is ~3,000+ non-test lines of a
  security-sensitive topology (credential isolation, exclusive volume lease,
  observe→create→re-observe→start atomic lease transfer, RO mounts,
  content-addressed instruction materialization). The A/B split moved the bulk
  into A rather than resolving it.
- Root cause: **duplication**. The "reusable" workspace/lease/collection
  primitives are vendor-neutral in behavior but are wrapped in `Codex*`-named
  methods on `*CodexReviewLifecycle` taking `Codex*` types
  (`codex_review_workspace.go:16`, `codex_review_volume_lease.go:14`), so there
  is no clean call-through reuse — only copy or refactor. Two independent
  ~3,000-line copies of one security topology must then be hand-synced forever;
  a future lease-race or mount-isolation fix landing in only one copy is a
  security bug.

## Rejected alternatives

- **Parallel Claude-named copies + dedup follow-up** (the plan comment's lean).
  Rejected: duplicates a security-critical topology; drift between copies is a
  security defect, not just debt. The contract chain's active hold on
  `codex_review_source.go` (see Sequencing) is a real added cost of the
  refactor that narrows but does not flip this call.
- **One large ~3,000-line `#865` PR.** Rejected: reintroduces, worse, the
  large-diff review-quality problem the `#845` split was meant to avoid.

## Provider seam (design)

Extract the vendor-neutral core and route what varies through a small provider
abstraction. What varies between Codex and Claude:

- **Image pin** — `ApprovedImage` (both pin a digest-pinned agent image).
- **Egress endpoints** — Codex `chatgpt.com:443`/`api.openai.com:443`
  (`codex_review.go:159-166`); Claude `api.anthropic.com:443`
  (`exec/claude/spec.go:61`).
- **Review command builder** — `codexReviewCommand` (`codex_review.go:1539`)
  with `--output-schema`/`--output-last-message`; Claude has no
  `--output-schema`, so it enforces the P0–P3 findings schema in the prompt and
  relies on strict-decode on the way out (`exec/claude/` CLI conventions).
- **Credential-delivery strategy** — the biggest divergence. Codex = two-file
  `auth.json`+`AGENTS.md` snapshot volume (RO) plus host-side OAuth
  enrollment/refresh (`AuthStoreLeaser`/`CodexAuthRefresher`/`CodexAuthState`,
  `checkCodexAuthReenrollment`, `acquireCodexReviewAuth`,
  `reserveCodexAuthStartAdmission`). Claude = a single RO
  `CredentialManifestSetupToken` mount delivering `token` →
  `CLAUDE_CODE_OAUTH_TOKEN` (`exec/claude/driver.go:68`, `spec.go:366`), no
  refresh, no re-enrollment, no snapshot volume. Model this as a
  credential-strategy interface the core calls; the OAuth machinery becomes the
  Codex strategy's plug-in, never core.
- **Provider / source labels** — `"openai"`/`"codex_local"` vs
  `"anthropic"`/`"claude_local"`.
- **Evidence + config-digest version tags** — `codex-review-result-v3`,
  `codex-review-configuration-v3` → per-provider version strings.

Vendor-neutral (extract into core): workspace snapshot + read-only proof
(`codex_review_workspace.go`), exclusive volume lease
(`codex_review_volume_lease.go`), strict-JSON collection decode with the
`domain.AllFindingSeverities` (P0–P3) gate and the `StartLine < 1` concrete
line-range rule (`codex_review_collection.go` + normalize in
`codex_review_source.go:712-821`), and the launch skeleton (host-only network +
CONNECT proxy, RO mounts, content-addressed instruction materialization,
observe→create→re-observe→start atomic lease transfer).

## Trust-boundary discipline (mandatory)

This is a behavior-preserving refactor on a credential-isolation +
volume-lease + RO-mount trust boundary, so per AGENTS.md it requires a
refute-first, **fuzzed old-vs-new equivalence** proof, not a diff-read:
reconstruct the pre-refactor Codex implementation (`git show <base>:<file>`)
and compare decision-for-decision over a fuzzed corpus for (a) collection
normalization / strict-JSON decode and (b) launch-spec mount + command + env
derivation. Record confirmed / rejected-by-verification / accepted-by-decision
findings in this note as implementation proceeds. Mechanical renames/moves land
in their own commits, each added to `.git-blame-ignore-revs`, so the
substantive parameterization diff stays reviewable.

## Sequencing

`#872 starts-after #702` (cross-round finding identity), which merged as PR
#870 at `6bf2c8b8` on 2026-08-21. The refactor is grounded on post-#702 `main`:
#702 moved finding identity into `domain/finding.go`, touching the
`codex_review_source.go` neighborhood the core extracts, so refactoring onto
settled code avoids reshaping a moving target. `#854`/`#855` (dormant
`kind:contract` deferrals: evidence dual-version acceptance, deleted-file/§7
location representation) will rebase onto `#872` when eventually scheduled.

Chain: `#872 → #865 → #845 → #846` (tracker #835).

## Revisit when

- The provider seam forces a change to a **shared interface**
  (`exec.ReviewSource`/`ReviewResult`) or a `domain`/`api` type — then it is no
  longer a `lane:ward` refactor but a serialized `kind:contract` split; stop
  and file it.
- `#854` or `#855` is scheduled and reshapes `codex_review_source.go`'s
  evidence/normalization surfaces before `#872` merges — rebase and re-run the
  equivalence harness against the new base.
- The fuzzed equivalence harness surfaces any Codex behavior change — the
  refactor is not behavior-preserving and must be corrected before merge.
