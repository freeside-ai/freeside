# 2026-07-14 17:38 — Deterministic merge-result audit (#61)

User-assigned; issue #61 carries the work contract. Adds
`scripts/merge-result-audit.sh` + regression suite, scripts CI, and the
unmanaged "Integration ordering and merge-result audit" policy: the
project-specific defense for the #47-reverts-#48 class (see
2026-07-14-1252-restore-api-ci-vacuum-binary.md).

## Decisions

- **Merge machinery over PR-body parsing.** The audit takes explicit
  base/head refs and allowed paths, builds the prospective result with
  `git merge-tree --write-tree` (no checkout mutation), diffs it against
  the exact base SHA, and gates every changed path (both rename sides)
  on a directory-boundary allowlist. Closed PR #50's free-form `Scope:`
  parser was rejected as incident evidence, not reused; its guard also
  could not see a byte-identical revert, which is content, not scope
  declaration. The dangerous topology is *ancestry* (fork from sibling,
  then revert); cherry-pick-then-revert nets to no diff and merges
  harmlessly — test case 11 builds the ancestry form.
- Distinct exit codes (0 pass / 1 out-of-scope / 2 conflict / 3 usage /
  4 unverifiable); merge-tree's exit 1 counts as conflict only after a
  rev-parse pre-gate and a tree-OID check (bogus args also exit 1).
- Bash 3.2 compatible (macOS system bash); NUL-delimited plumbing with
  an end-of-diff sentinel so a failed diff-tree can't read as empty.
- CI pins shellcheck 0.11.0 by sha256 (vacuum pattern); `actions/*@v4`
  stays tag-pinned, matching the existing workflows.

## Refute-first pass (destructive-path class; fresh-context lens)

- **Confirmed, fixed:** `submodule.<name>.ignore=all` (repo config or
  in-tree `.gitmodules` on the audited branch) cloaked gitlink moves
  from diff-tree → false PASS. Fixed with `--ignore-submodules=none`;
  regression case 15.
- **Confirmed, fixed:** a tag shadowing a branch/remote name resolves
  in the tag's favor (`--quiet` hid the warning) → audit examines the
  wrong commit. Ambiguous names now exit 4; regression case 14.
- **Confirmed, partially adopted:** audit inherited user/global git
  config; it now nulls `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM`.
  Repo-local config and in-tree attributes remain trusted input
  (documented in the header) beyond the two closed cloaks.
- **Confirmed, fixed (Codex review, P2):** a local `refs/replace/`
  entry substitutes objects during merge-tree/diff-tree reads while
  rev-parse still prints the original SHA, so the recorded SHA is not
  the audited content → false PASS (reproduced). Fixed with
  `GIT_NO_REPLACE_OBJECTS=1`; the class sweep also rejects shallow
  repositories, whose truncated history can yield a different merge
  base than the forge's. Regression cases 16–17.
- **Confirmed, fixed (Codex review, P2, round 2):** an in-tree
  `.gitattributes` merge attribute plus a local `merge.<name>.driver`
  config makes merge-tree resolve a both-sides edit to base content,
  erasing the out-of-scope path from the change set the forge would
  conflict on: the concrete form of the refute pass's "possible" local
  merge-driver finding. Locally configured drivers now exit 4
  (global/system config is already nulled, so any visible driver is
  local state the forge lacks). Regression case 18.
- **Confirmed, fixed (Codex review, P2, round 3):**
  `merge.directoryRenames=false` in `.git/config` hid a
  directory-rename conflict the forge would report → false PASS: third
  member of the repo-local-merge-state class, so the boundary was
  widened from per-key rejection to the whole family: any local
  `merge.*` config or the `diff.renames`/`diff.renameLimit` fallbacks
  merge-ort reads now exit 4. Regression case 19 (case 18's driver
  check is subsumed by the same reject).
- **Confirmed, fixed (Codex review, P2, round 4):** `.git/info/grafts`
  fakes parents during ancestry walks (deprecated precursor of replace
  refs, not covered by `GIT_NO_REPLACE_OBJECTS`), moving the merge base
  and re-hiding the case-11 reversion → false PASS. A present graft
  file now exits 4. Regression case 20.
- **Confirmed, fixed (Codex review, P2, round 5):**
  `$GIT_DIR/info/attributes` marking a file `merge=union` turned a real
  conflict into a clean PASS (conflict-detection guarantee broken by
  local state). Fixed by closing every out-of-tree attributes source:
  `GIT_ATTR_NOSYSTEM=1`, `core.attributesFile=/dev/null` pinned on the
  merge-tree/diff-tree invocations (overrides the XDG default and any
  local config), and a present `info/attributes` rejected. In-tree
  `.gitattributes` remains in effect: reviewable, forge-visible
  content. Regression case 21.
- **Confirmed, fixed (Codex review, P2, round 6):** merge-tree reads
  attributes from the *current checkout*, so a `.gitattributes`
  committed on the head branch steered conflict detection when the
  audit ran from the PR worktree (the normal case) while a base-side
  merge conflicted. Fixed with `--attr-source=$BASE_SHA` (git >= 2.40;
  older git fails loud on the unknown option): attribute semantics are
  bound to the exact base tree, the audit is checkout-independent, and
  head-committed attributes take effect only once merged. Regression
  case 22. (Round 7 corrected the documented minimum to git >= 2.41,
  where --attr-source actually shipped.) This supersedes round 5's "in-tree attributes remain in
  effect" wording: in-tree means the *base* tree.
- **Confirmed, fixed (Codex review, P2, round 8):** a partial
  (promisor) clone lazily fetches missing blobs over the network during
  merge-tree/diff-tree, breaking the no-network guarantee and mutating
  the object store mid-audit. Promisor-configured clones now exit 4,
  with `GIT_NO_LAZY_FETCH=1` as backup on gits that honor it.
  Regression case 23.
- **Accepted-by-decision (threat-model boundary):** the local-state
  rejects (replace refs, grafts, shallow, merge config, submodule
  ignore) close the *accidental and observed* divergence classes
  between the audit's merge and the forge's. A fully hostile local
  `.git` (e.g. a forged commit-graph) is out of scope: an attacker who
  writes `.git` can edit the audit script itself, so in-script defenses
  cannot hold there; the forge-side prospective diff is the eventual
  mechanical home (plan §5.6 importer).
- **Accepted-by-decision:** floating `actions/checkout@v4` (consistency
  with daemon/api CI; changing the action-pinning convention is its own
  unit).
- **Rejected-by-verification (attacks that failed):** typechange,
  mode-only change, glob metacharacters in allowlist entries, `api`
  vs `api2/` boundary, newline-in-path, partial-similarity rename,
  union merge attribute, criss-cross bases, empty-array-under-`set -u`
  on bash 3.2, corrupt-object/truncated-plumbing states.

## Deferrals / queue

- Queue swept: open items (`approved-recipe-boundary` → #52,
  `domain-package` review → #27, the 1519 write-boundary candidate) are
  store/domain docs promotions outside this unit's scope; none drained.
- PR template's Verification comment grows a Freeside-specific audit
  instruction: intentional further divergence from the canonical
  scaffold (like the existing `## Scope` section).
