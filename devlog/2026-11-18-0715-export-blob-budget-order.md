# Spend the Export Blob Budgets on Unignored Files First

In #2382's live run, #2968's implementation run built `app/` and left about
12,400 files of SwiftPM output under `app/.build/`, which `app/.gitignore`
ignores. The exporter wrote a blob for each, the handoff passed the ward's
10,000-entry cap, and the whole handoff failed, so the agent's real change
never reached import. #3347 fixes the exporter. This note records why. The
work is Tier A by path (`internal/export/`, `internal/ward/`).

## Decisions

- **Omit blobs past a count budget rather than raise the ward cap.** The
  owner chose this. The exporter now stops writing new blobs at
  `export.DefaultMaxBlobs` (9,000) and records further files as
  `blob_omitted` with their digest, as its byte budgets already did. That
  leaves 1,000 of the ward's 10,000 entries for the output's directories,
  manifest, commit plan, and evidence channel, and a ward test pins the
  fit. Raising the cap was rejected: it bounds what the host extracts from
  untrusted output, and any fixed cap is a number some build exceeds. An
  omission is the contained failure direction, since import blocks on an
  omitted blob it needs.
- **Order the budgets by the workspace's own `.gitignore`.** The owner
  chose this over a plain count budget in manifest order. Sorted order puts
  `app/.build/` before `app/Sources/` and `daemon/`, so a plain budget
  would spend itself on build output and withhold the change, and import
  would block on it. The exporter now offers blobs to `.gitignore` files
  first (shallowest first, since their rules decide the rest), then to
  files those rules leave unignored, then to ignored files. Every file is
  still read once and recorded; only which blobs are omitted moves.
- **The exporter now parses `.gitignore`, and only as a hint.** This
  amends the package's rule that it never parses workspace content. The
  rules are agent-controlled, and that is acceptable because the hint
  grants nothing: it only reorders the budgets. The importer still judges
  ignored-ness against the exact base (#3319), and blocks on an omitted
  blob for any changed entry the base doesn't ignore. So a wrong or hostile
  hint can at worst cause a blocking omission, which the agent can already
  cause by writing more files. The bytes come from the same single read
  that makes the blob, capped at 64 KiB per file and 1,000 rules across
  the workspace. Reading the base's rules instead was rejected: the
  exporter has no base checkout, and giving it one widens a container
  that is deliberately given only the workspace.
- **Match git's rules for the forms build tools write.** The matcher
  handles comments, negation, directory-only and anchored patterns, `**`,
  rules files at any depth with deeper files overriding, and git's rule
  that nothing re-includes a file under an ignored directory. It doesn't
  read `.git/info/exclude` or `core.excludesFile`, which the walk never
  visits. Every table case matches `git check-ignore` on the same tree.
  A divergence only reorders the budgets, so near-parity is enough.

## Review Findings

A fresh-context refute pass found no path where the hint grants anything
or pushes the output past the ward cap. Its findings:

- **Confirmed and fixed: git's `[!x]` class was read inverted.**
  `path.Match` takes only `^` as class negation, so `*.[!o]` ranked
  `a.o` last and `a.c` first. The matcher now rewrites `[!` to `[^`.
- **Confirmed, partly fixed: a hostile tree can make matching slow.** A
  64 KiB `**/a/` rule against a 2,000-deep path took 28 s per file, and
  the doc claimed the caps bounded the cost. Rules over 64 segments are now
  dropped, each file's path is split once, and directories without rules
  are skipped, so that case is gone. What remains is a cost of rules times
  segments times depth for each new directory: a few deep directories and
  many rules can still push the export toward the exporter timeout. A
  global work budget was rejected: the timeout fails only the hostile
  agent's own handoff, which it can already cause in other ways, and a
  budget adds a second, order-dependent hint behavior. The doc now states
  the cost plainly.
- **Accepted: a file the base tracks but its rules ignore now ranks
  last.** If it changed and the budget runs out, import blocks where
  sorted order might not have. That fails closed, and the byte budget
  already allowed the same outcome.
- **Disproved:** the count cap can't be exceeded (each store adds at most
  one blob, checked before it); the `.gitignore` capture can't grow past
  the file's recorded size; the order is deterministic; and every regular
  file is still digested exactly once.

## Revisit When

- A real workspace's unignored files alone exceed the blob budget. Then
  the export omits part of the change and import blocks, and the fix is
  a larger ward cap or a different handoff shape, not a better order.
