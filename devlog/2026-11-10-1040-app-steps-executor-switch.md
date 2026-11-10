# Freeside's Recipe Reaches the Executor Through a check.sh Switch

Work unit: #2883. Mandatory note: Tier A, because the unit changes what
`TestImportFreesideProtectedPaths` asserts about the import gate's
control-plane hold (AGENTS.md, Assurance Tiers, Classification). The 2026-11-08
planning note in the issue read the unit as Tier B. The 2026-11-10 planning
revised that, and this note records the revision. The rest of the change (the
recipe, `check.sh`, and the README) is ordinary work.

## Decision

- **The executor flags sit behind `APP_MACVERIFY` in `scripts/check.sh`.**
  The executor's room needs `app test`, `app build-mac`, and `app build-ios`
  to turn off SwiftPM's and Xcode's own sandboxes and to read the job's seeded
  `../source-packages` and write `../derived-data` (daemon/README.md, Install
  the macOS Verification Executor). Chose an opt-in switch over two
  alternatives:
  - *Flags in the recipe.* `verify.ParseRecipe` refuses any token with a `..`
    path segment, and the seed sits beside the workspace, so the recipe can't
    name it. Loosening that refusal for one recipe would weaken a Tier A
    parser to serve a convenience.
  - *A separate executor script.* A second script would be a second copy of
    the app steps, and the two copies would drift. The switch keeps one
    definition of each step.

  With the switch unset, every step's command line is byte-for-byte what it
  was, so CI is unchanged. `scripts/test-check.sh` pins both argument lists
  in full. The switch builds the job directory from the checkout root's
  parent, an absolute path, because the steps run from `app/`.
- **`.github/**` is automation control in Freeside's onboarding.** The
  defaults cover the workflows, actions, and a few named files, but not the
  pull request template, which shapes what reviewers read. So Freeside widens
  the class to the whole directory. That also makes the reviewer-instruction
  paths under `.github/` (`copilot-instructions.md`, `instructions/**`,
  `agents/**`, `skills/**`) hold publication rather than only surface, which
  is intended: they steer agents as much as the template steers reviewers,
  and Freeside has none today. The test now shows the template drawing
  a blocking finding that fails without the widening, and `app/` and `devlog/`
  edits drawing none, since the deputy may now edit them.
- **Agent-instruction files stay allowed, not protected.** By owner decision
  on 2026-11-08, `AGENTS.md`, `CLAUDE.md`, `.agents/**`, and `.claude/**` are
  ordinary allowed paths. Their built-in advisory reviewer-instruction finding
  still surfaces every edit. The test leaves them out of its unflagged list
  because that finding is outside its `want` set.
- **`images/` stays out of the allowed paths.** The agent base images need
  the network to build and are part of the trust boundary, so they stay
  operator-built.
- **The slot proof of the Apple steps moved to #3016 (owner, 2026-11-10).**
  The contract asked for all four `macos_host` commands to pass as executor
  slots. Chose to land the recipe with only `app format` proven as a slot,
  over holding the unit until the executor works, because nothing can run
  these commands before #3009, #2978, and #3013 land, and #2382's
  onboarding waits on all three. The slot proof then follows the fix that
  makes it possible.

## Evidence

- The first real install and slot run of the macOS executor, on the
  reference host on 2026-11-10, found four defects that keep a slot from
  completing a job (#3009). `app format` passed as a slot, but cleanup then
  failed on SIP-protected entries trustd and the Metal compiler service
  leave in the slot's temp directory, and the slot refused every later job.
- In the mixed Go and Swift image, `daemon test` and `scripts` fail at
  `main` as well as at this unit's head. Six daemon tests and one
  `test-check.sh` case assume things the proof's workspace doesn't give them
  (#3013). `app linux` passes. Lint's unreclaimable peak rose to 1600 MiB,
  past the 1536 MiB margin of
  `devlog/2026-10-22-1241-proof-memory-bound.md` but inside the room.

Revisit when: the recipe parser gains a way to name the job directory, or the
room runs commands from the job directory instead of the workspace. Either
would let the recipe carry the flags itself.
