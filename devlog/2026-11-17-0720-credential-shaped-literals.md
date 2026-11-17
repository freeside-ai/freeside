# Keep Credential-Shaped Literals Out of Tracked Files

Found by #2382's live run, the first to run a specifier on Freeside's own
repository (#3305).

## Finding

- **The export scan refused a handoff that leaked nothing.** The specifier
  for #2968 lost its run after two minutes: `export_verification: output
  scan refused the export (details withheld)`. The repository tracked 35
  lines in 28 files that `internal/credentialpattern` matches, most of them
  test fixtures (`sk-ant-…`, `ghs_…`, PEM headers) and one in production
  code (`publish/onboarding.go`'s RSA PEM header).
- **The agent's transcript is part of the export.** An agent that reads or
  greps one of those files echoes the match, and the scan refuses the whole
  handoff. The importer applies the same rules to specifications, replies,
  and publication prose, so a quoted fixture can refuse there too.
- **Not confirmed for that run.** The refused export was deleted and the
  scan's detail withheld (#3306), so which rule and file matched is
  inferred, not observed.

## Decision

- **Tracked files carry no line the credential rules match.** Each fixture
  is built at runtime with a byte-identical value: a Go literal splits
  after its prefix (`"sk-ant-" + "…"`), a JSON fixture escapes one
  character (`ghs_…`), and the signing key keeps only its base64 body,
  with the armor added in `fixtureKeyPEM`. No test changes what it feeds a
  gate or asserts.
- **A `required` check holds it.** `scripts/check-credential-literals.sh`
  pipes `git ls-files -z` to `daemon/cmd/freeside-credential-literals`,
  which applies `credentialpattern.All()` itself, so the check and the two
  gates read one rule list. It runs on every pull request through its own
  workflow, since a literal can arrive in any file, and it prints
  `path:line: rule-id`, never the match.
- **Rejected: a shell copy of the patterns.** Two hand-kept copies of the
  rules drifted once (#2773); a third would too.
- **Rejected: exempting testdata or `_test.go` files.** An agent reads tests
  as readily as code; an exempt fixture refuses the export all the same.
- **Not decided here: exempting content that's verbatim in the seeded
  base.** That would change what the export scan refuses, a Tier A policy
  question #3305 leaves open; the cleanup doesn't depend on it.

## Refute-First Pass

- **A fixture's value changed: disproved.** Go folds the split constants at
  compile time: `internal/credentialpattern`'s test binary came back
  `(cached)` after the edit, so it compiled identically. The reassembled
  signing key is byte-identical to the deleted file (`cmp`), and
  `encoding/json` decodes the escaped fixtures to the old strings. Every
  affected package passes.
- **`ImportKey` accepts or refuses differently: disproved.** The
  `internal/publish` package's assembly (`-gcflags=-S`) is identical on
  `main` and this branch once file:line references are removed, about 454k
  lines with no diff.
- **The check leaks what it finds: disproved.** It prints the rule ID and
  line number only; `TestReportsEachMatchByFileLineAndRuleWithoutTheMatch`
  asserts the matched text never reaches stdout.
- **The check misses a newline-spanning rule: disproved.** It matches
  whole file content, so the GCP rule's `\s*` still matches across lines
  (`TestMatchesARuleThatSpansLinesAtItsFirstLine`).
- **The check isn't a strict superset of the scan: confirmed, accepted.**
  The scan reads 1 MiB chunks, and a chunk edge counts as a `\b`
  boundary, so a rule ending in `\b` (`aws_access_key_id`, `github_pat`,
  `slack_token`) can match a prefix there that whole-file matching passes,
  such as a test's `AKIA` run longer than 16 characters. The odds are
  about two in a million per occurrence per scan, too low to make the
  check chunk-aware.
- **Git history still carries the removed literals: confirmed, accepted.**
  An agent that runs `git log -p` or diffs across this change echoes them.
  The fix keeps the tree clean, which is what an agent reads in ordinary
  work; history stays a gap until the base-verbatim question above is
  decided.

## Revisit When

- A rule joins `credentialpattern`: the check enforces it at once, and any
  fixture it newly matches needs the same treatment in that PR.
- The export scan gains a base-verbatim exemption, which would let the
  check relax to content agents can't read.
