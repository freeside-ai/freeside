# Clean verifier: precise command-path classification

Work unit for #149 (gauntlet): make `Recipe.CommandPaths()`
(`daemon/internal/verify/recipe.go`) classify recipe argv tokens as
verification-control file paths precisely, so a non-file operand that
merely contains `/` is no longer read as a repo path. Surfaced by Codex
P2 review of PR #147 and deferred there
(`2026-07-17-1200-verify-argv-nongo.md`, "CommandPaths lexical-extraction
imprecision"). Trust-boundary change (returned-object /
path-classification), so a refute-first pass was run before commit.

## Decision

- **Classify via whitespace-exclusion.** A command token that carries any
  whitespace (`unicode.IsSpace`) is treated as a multi-word argv operand,
  not a single filename, and is dropped from `repoRelPath` classification.
  Opaque argv packs a multi-word operand into one token
  (`-destination "generic/platform=iOS Simulator"`, a `sh -c` script
  string `scripts/verify.sh --fast`); reading such a token as a repo path
  both over-flags the operand and, when its first segment collides with a
  repo symlink, spuriously trips `rejectSymlinkEntrypoints`
  (`ErrSymlinkEntrypoint`). The two-line guard sits alongside the existing
  separator / absolute / `...`-segment checks; the git-tree symlink guard
  is untouched.

  This works because the two acceptance shapes are lexically identical at
  the tree level (`generic/platform=iOS Simulator` and
  `run-check/verify.sh` are both `<symlink>/<tail>`); the only tree-level
  discriminator would be whether the tail resolves through the symlink.
  Whitespace sidesteps that: the destination operand has a space, the real
  entrypoint does not.

- **Owner choice (this session):** whitespace-exclusion over the precise
  alternative; and the shell-`-c` under-flag deferred rather than closed
  now (see Follow-up).

## Rejected alternative

- **Tree-existence classification with symlink-target resolution.** Walk
  each token's segments against the head tree, resolving symlink targets,
  and classify only tokens that resolve to a real tree entry, folding the
  symlink-entrypoint guard into that same walk (crossed-a-symlink → fail
  closed). Precise for any operand shape (URLs, regexes, spaced paths) and
  has no whitespace-shaped hole. Rejected: ~50-80 lines of git symlink
  resolution (cat-file + re-root + loop/depth bounds) on a security path,
  plus a fuzz corpus, for a latent/rare bug; it also softens the prior
  "resolving symlink chains is disproportionate" stance (`verify.go`
  `rejectSymlinkEntrypoints` doc). Disproportionate to the harm.

## Accepted limitations (by decision, not defects)

- A repo file whose path contains whitespace, used as an entrypoint, is no
  longer classified. The recipe author names entrypoints without spaces;
  no current or planned recipe has a whitespace-bearing entrypoint.
  (Pinned by `TestCommandEntrypointFilenameEnumeration`'s carve-out.)
- A non-file operand with `/` but no whitespace (a URL, a regex) still
  classifies spuriously. Latent: no current recipe passes such an operand.
- A spaced symlink entrypoint would bypass the symlink guard
  (defense-in-depth gap). Only a trusted recipe author could author it;
  not a plausible mistake.

## Verification findings

- Refute-first pass (fresh-context reviewer, diff + issue intent only,
  tasked to disprove): **no new defects beyond the three accepted
  limitations.** Attack classes ruled out: (1) no smuggling route where a
  whitespace-free token resolves to a symlink the guard misses — the only
  unclassified path is a whitespace-bearing token, which is limitation (a)
  by definition; (2) `path.Clean` never adds/removes whitespace runes, so
  `ContainsFunc` running before `Clean` cannot desync classification from
  the downstream literal `foldPath` match; `..` is already rejected in
  `validateCommand`; (3) `unicode.IsSpace` is broader than the doc's
  "space/tab/newline" but every extra rune only moves *more* tokens into
  the operand bucket, never drops an intended plain entrypoint; (4) the
  two flagged tests are non-vacuous — each flips (fails) if the fix is
  reverted, confirmed by trace.
- `go test ./internal/verify/`, `go vet`, `golangci-lint run`
  (0 issues), `go build` all pass.

## Revisit when

- A recipe needs a whitespace-bearing entrypoint path (revisit the
  whitespace rule, likely toward tree-existence classification), or
- a shell-runner `sh -c` recipe becomes reachable (the deferred under-flag
  below becomes live).

## Follow-up

- #154 (gauntlet, `deferral`, `kind:fix`): extract or reject the real
  entrypoint embedded in a shell-runner `-c` string, so a candidate edit
  to it is flagged. Whitespace-exclusion already removes the shell
  string's spurious-guard / over-flag harm; the residual is the
  under-flag. Latent until a shell-runner recipe exists.
