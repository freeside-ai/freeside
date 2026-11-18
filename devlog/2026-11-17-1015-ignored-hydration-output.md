# Exclude Ignored Hydration Output at Import

The owner chose, during the #2382 exit run, to revise the #1132 rule
(`2026-09-09-1040-ignored-build-output.md`) instead of working around it.
#3319 applies the revision as plan revision 129.

## Decision

Chose importer-side exclusion of every addition the exact trusted base
ignores and doesn't track, under every finding profile, over the #1132
rule's narrower set. An addition qualifies when it's a new regular file
(including one whose blob the export caps withheld) or a new nested
repository (a submodule entry), in scope or not, and the base tracks
nothing at or beneath its path. For an excluded addition, the importer
drops the findings that only describe content no commit will hold:
allowlist, `blob_omitted`, the submodule `non_regular_change`, a per-file
`size_violation`, and `secret_scan_skipped`. It keeps a secret match on
scanned content, the protected-path and git-metadata findings, collisions,
and the change set's total size. A handoff that must carry no change keeps
every addition and still refuses.

## What Changed Since #1132

#1132 assumed ignored debris was a new regular file out of scope, left by
an agent. The #2968 specification run disproved all three parts:

- **The project image makes the debris.** Swift preparation runs
  `swift package resolve`, which leaves 12 nested repositories under
  `app/.build/checkouts/` and bare repositories under
  `app/.build/repositories/`, including a 120 MB pack. The export records
  the nested repositories as submodule entries and withholds the pack's
  blob, so the import failed on `non_regular_change` and `blob_omitted`,
  both inherently fatal, before any agent edit mattered.
- **The debris can be in scope.** #2968's allowlist covers `app/**`, so the
  out-of-scope condition kept `app/.build/` content as a candidate change.
  "In-scope ignored files may be intentional new content" no longer holds
  as a default: content a person meant to add must be tracked or
  un-ignored, which the base's own rules decide.
- **Specification imports see it too.** The specification profile tolerated
  scope and scan-cap findings but never the inherently fatal ones, so
  keeping it out of the exclusion left it blocking.

## Rejected

- Tolerating these findings in the specification profile only: the same
  debris would then block publication.
- Hydrating outside the workspace: it needs a per-toolchain cache-location
  contract and leaves every other toolchain's ignored output blocking.
- Dropping the secret or total-size findings on excluded content: a secret
  the scan saw is evidence of exposure whether or not it's committed, and
  the total cap bounds what the daemon handles, not what it commits.

## Refute-First Findings

From a fresh-context refute pass before commit, plus the author's own checks.

- Confirmed: Git judges a directory by its own name only when that name is
  a directory on disk. Queried as a file, a nested repository misses a
  directory-only rule such as `/vendor/`. Queried as `sub/`, Git tests an
  empty basename that `*` matches even under `!sub/`, so a re-included
  repository looked ignored. Nested repositories now match in their own
  scratch tree, each created as an empty directory, apart from candidate
  files so a created directory never changes how a file is judged. Their
  names join the spelling-alias check, and Git-metadata components refuse.
  `TestImportExcludesIgnoredHydrationDebris` and
  `TestIgnoreMatcherHonorsReincludedNestedRepository` cover both.
- Confirmed: `check-ignore --stdin` reads pathspecs, so a candidate named
  `:(top)dist/out.js` matched as `dist/out.js` and was excluded. This
  predates the revision but now drops more findings, so a name starting
  with `:` is never eligible (`TestIgnoreMatcherRefusesPathspecMagic`).
  `--literal-pathspecs` makes Git refuse the input, not read it literally.
- Confirmed: excluding a nested repository over tracked content would hide
  the tracked files its opaque subtree retains. Additions at a tracked
  directory stay candidates (`TestImportIgnoredNestedRepositoryStillBlocks`).
- Disproved: excluded content reaching a commit (construction and the
  commit plan read only the filtered changes, and a plan naming an excluded
  path falls back to one commit); a kept finding kind dropping (the filter
  needs both an excluded path and a dropped kind, and excluded paths are
  only untracked regular files and nested repositories); case aliases
  hiding deletions (opaque suppression is byte-exact).
- Accepted: replay under a new build can differ from a recorded head that
  included an in-scope ignored addition. Publication compares the replayed
  commit with the recorded head and refuses on a mismatch, so a run in
  flight across the upgrade fails closed rather than publishing.
- Accepted: an in-scope file the base ignores now leaves the candidate with
  no notice in the import result. The base's own rules decide; a person who
  means to add such a file must track it or change the rule.

A real `swift package resolve` workspace at the #2968 base, exported by the
real exporter, imports with no findings under both profiles and #2968's
allowlist.

Revisit when a project needs ignored content published on purpose, or when
an export transport limit makes returning ignored bytes for scanning
impractical.
