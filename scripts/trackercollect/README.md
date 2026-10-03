# Trackercollect

`trackercollect` gathers advisory, stamped GitHub evidence for Freeside's
post-merge tracker reconciliation. It writes `snapshot.json` and `report.md`;
it never writes to the forge and does not decide wave state, path overlap,
startability, or mergeability. Its `contracts` subcommand reports contract
occupancy instead; see Contract Occupancy below.

The runtime requires Go 1.26.6 and an authenticated `gh` CLI with access to the
target repository. Run it from this module:

```sh
go build -o /tmp/trackercollect .
/tmp/trackercollect --repo github.com/freeside-ai/freeside --pr 910 --out /tmp/trackercollect-910
```

The pull request must already be merged. The command exits 0 for a complete
collection, 2 after writing artifacts that contain one or more `AMBIGUOUS`
entries, and 1 for a hard failure. A non-merged pull request fails before the
output artifacts are written. A prompt-backed direct unit has no forge issue
that proves its origin, so pass `--direct` to assert that provenance. Without
the flag, zero attributed closing issues produce an `AMBIGUOUS unit-origin`
entry; using the flag when the forge attributes a closing issue is a hard
failure.

Run the built binary when its exact exit code is required. `go run` reports a
child exit status but returns its own nonzero status, so it cannot preserve the
collector's distinction between exit 1 and exit 2.

Every GraphQL connection has a finite page cap. Reaching it preserves the
partial evidence, records an `AMBIGUOUS` truncation entry, and returns exit 2.
Before using a snapshot for a tracker edit, follow the inventory-aware
freshness recheck in `docs/coordination.md`.

## Contract Occupancy

`trackercollect contracts` reports which open `kind:contract` issues hold an
active claim and whether the `coord:contract-active` label matches. It is
read-only: it reports label drift and people repair it. The label's lifecycle
is in `docs/coordination.md` (Contract Occupancy Label); the cap and the claim
gate it supports are in `AGENTS.md` (Contract Changes).

```sh
/tmp/trackercollect contracts --repo github.com/freeside-ai/freeside --out /tmp/contracts
```

`--cap N` sets the cap the active count is printed against (default 2). The
command writes `contracts.json` and `contracts.md`. Each open contract issue
gets one state, the first that applies:

| State | Rule |
| --- | --- |
| `active-pr` | An unreleased claim comment, of any age, whose branch has an open PR from the canonical repository that closes the issue. |
| `active-lease` | An unreleased claim comment less than 48 hours old. |
| `active-pr-legacy` | An open PR that closes the issue with no unreleased claim comment behind it. |
| `reserved` | A planning reservation comment less than 48 hours old. |
| `dormant` | A `deferral` with no milestone. |
| `expired` | An unreleased claim comment 48 hours old or older. |
| `idle` | None of the above. |

A claim is released only by a release comment on the same issue whose
`Releases-claim` names the claim comment's ID. A PR closes the issue when the
forge lists that issue, matched by node ID, among the PR's closing issues, or
when the PR body carries a close keyword for its number. The body is read
because the forge lists no closing issue for a PR based on a branch other
than the default one, which is every stacked PR. A keyword quoted in a code
block, or in a code span that opens and closes on one line, closes nothing;
a span that crosses a line break is read as plain text. An open PR from a
claimed branch that does neither is named and keeps no lease alive. For an
`active-pr` or `active-lease` unit the report prints the ordering claim: the
earliest unreleased, unexpired claim comment by `created_at`, then ID.

A marker counts only when the forge gives its comment's author association
as `OWNER`, `MEMBER`, or `COLLABORATOR`. Anyone can comment on a public
repository's issues, and the claim protocol trusts collaborator comments
without saying what anyone else's marker means. The report doesn't settle
that: a claim, release, or reservation marker from any other author is not
counted and adds an `AMBIGUOUS untrusted-marker-author` entry naming the
comment and its association. The merged-PR collection above reads a marker
whoever posted it.

The three `active-*` states count toward the cap and expect the label.
`reserved` is counted separately and expects none. The findings are
`missing-label` (an `active-*` unit without the label) and `stale-label` (the
label on any other unit). An active count over the cap is stated and is not a
finding. For each unit the report names the open trackers whose Units section
lists it.

Exit codes: 0 for complete evidence with no findings, 3 for complete evidence
with findings, 2 when any `AMBIGUOUS` entry exists (artifacts are still
written), and 1 for a hard failure, including a usage error. With any
`AMBIGUOUS` entry the report states no active count, finding count, or clean
verdict, and `contracts.json` carries `complete: false` with null counts. The
entries are a truncated inventory, PR list, closing-issue list, or comment
page, a malformed marker comment, a marker comment from an author who is
not an owner, member, or collaborator, an issue with more labels than one
page holds, and a tracker whose Units section is missing, repeated, or holds
an invalid issue number.

Known limits:

- A released reservation is one whose comment no longer carries the
  reservation marker. A reservation comment that keeps the marker reads as
  `reserved` until it is 48 hours old.
- Only open PRs are read. A lease whose PR closed unmerged inside its 48
  hours still reads `active-lease`, so it can show a false `missing-label`.
- A released claim whose PR stays open with the close keyword reads
  `active-pr-legacy`: the PR would still close the issue on merge.
- A claim posted through a GitHub App or bot account reads as `AMBIGUOUS`
  when the forge gives that account an association outside the trusted
  three. No claimant posts that way today.
- The author check covers marker comments only. An open PR that closes
  the issue counts toward `active-pr-legacy` whoever opened it, from a fork
  included.
- Only the forge's current state is read. A claim whose PR arrived, or
  gained its close keyword, after the claim's 48 hours reads `active-pr`
  with the claim comment as its ordering key, though the protocol calls that
  lease dead. The unit is active either way; only the ordering key differs.
- A legacy `Claim #N` commit is not read: commit messages are never
  fetched. An open PR that claimed its unit only that way would read
  `idle`. None was open when this mode was added, and the protocol allows
  no new claim commit.
- A marker is read only in its canonical form: the marker alone on a line,
  outside a code block or list item. Any other form is not a marker and adds
  no `AMBIGUOUS` entry.
