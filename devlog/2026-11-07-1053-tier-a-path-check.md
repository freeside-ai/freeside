# The Tier A Path List Is Checked on Every Pull Request

AGENTS.md's Tier A Paths list stays a hand-written list, and
`scripts/check-tier-a-paths.sh` now fails any pull request whose list names
a path that matches nothing (#2757). Chose a resolution check over leaving
the list to review alone, because a stale entry fails silently: a rename or
deletion drops the moved code from the Tier A bar and nothing notices.

## Evidence

- **One entry was already stale.** An architecture review on 2026-11-07
  found `internal/engine/production_unpublished_reentry.go` still listed
  after 567421d46 removed it. Of the section's 97 entries, it was the only
  one that matched no file.
- **One gate file was missing.** `internal/engine/fake_publication.go`
  pushes heads and calls `publish.Publisher.PublishAfterGateAndFinalize`,
  so the "unlisted file joins the list" rule already made it Tier A by
  category (GitHub publish). It is now listed under "Publication decisions
  in the engine".

## Decisions

- **Hand list kept; tiers in code deferred.** Classifying tiers in code
  (a manifest or package annotation) would remove the list, but it changes
  how the policy is written, so it is the owner's call on the review
  tracker. The check makes the current form safe to keep meanwhile.
- **Unconditional job over a selected component.** The list breaks from
  a `daemon/` rename or an `AGENTS.md` edit, and neither selects the
  `scripts` component (its paths are `scripts/`, `app/scripts/`,
  `.githooks/`, and two app files), so a check there would never run on
  the PRs that break it. A path-selected job would need its own list of
  trigger paths, one more list to keep in sync. The `tier-a-paths` job runs
  on every PR, as `migration-history` does; it reads one file and stats
  about a hundred paths, so the cost is a runner start.
- **Bare names resolve against the bullet's directory.** The list writes
  "in `cmd/freesided/` the files `auth*.go`, ...". The check resolves a bare
  `.go` or `.sql` name against the most recent directory entry in the same
  bullet, and fails one with no directory before it, so a reworded bullet
  can't make a name pass against the wrong directory. Other backticked words
  (Go identifiers such as `CredentialMode`) are skipped.
- **A reshaped section fails loudly.** A missing heading, or a section with
  no path entries, exits 2, so renaming the heading or turning the list into
  a table can't make the check pass vacuously. The parser reads list items
  as Markdown does (any list marker, lazy continuation lines, multi-backtick
  spans), and only a heading of level 3 or above ends the section, so a
  reflow or a subsection can't hide the entries after it.

## Limits

- **Resolution, not completeness.** The check proves each entry names a
  file; it cannot find a gate file the list omits, as `fake_publication.go`
  was. That stays with review and the category rule.
- **The filesystem, not git.** An untracked file can satisfy an entry
  locally; CI's clean checkout is authoritative.

Revisit when tiers are classified in code, or when the Tier A Paths section
stops being a bullet list of backticked paths.
