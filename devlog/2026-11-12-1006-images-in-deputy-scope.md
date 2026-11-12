# Freeside Tasks May Edit `images/`

Issue #3130; plan revision 120; tracker #3132. A material plan decision, so
a mandatory note.

Chose to bring `images/` into the deputy's allowed paths, with the agent
bases still operator-built, over keeping image work hand-run, because the
owner's rule is that every kind of development done by hand goes through
Freeside, and nothing in Freeside builds from `images/`, so letting a task
edit it grants no new authority.

## Decision

- **A task may edit `images/`.** The Containerfiles,
  `images/agent-claude/managed-settings.json`, and the READMEs. The build
  scripts live in `scripts/`, which was already allowed. The deputy's
  `repository.allowed_paths` and the submission policy's `paths` gain
  `images/**` together, because admission requires the two lists to match
  exactly.
- **The agent bases stay operator-built.** No Freeside path reads a file
  under `images/`: the project-image builder writes its own Containerfile
  on the admitted base digest. The deputy runs only the digests the
  operator pins in its `images` settings, and preflight refuses an agent
  image whose CLI disagrees with the daemon's compiled pin. So a merged
  image change reaches use only when the operator builds, probes, and
  re-pins it.
- **No new importer finding or protected path for `images/`.** An image
  edit publishes like any other unprotected path.
- **CI runs the daemon tests on an agent-base change.**
  `TestHarnessBuildsMatchTheImagePins` and
  `TestClaudeImageDisablesEveryBuiltinPlugin` read the image source, but
  the `required` check didn't select the daemon component for an
  `images/`-only pull request, so a pin bump that broke them could reach
  `main`. Both `images/agent-claude/` and `images/agent-codex/` now select
  it.

## Direction

- **Owner, 2026-11-11 (tracker #3132):** every kind of development done by
  hand today goes through Freeside.

## Overturns

- **Revision 113 item 1's `images/` exclusion**, and the matching choice in
  [`2026-11-08-1223-whole-repository-self-management.md`](2026-11-08-1223-whole-repository-self-management.md)
  ("`images/` holds the agent bases, which stay operator-built"). The
  operator-built half stands; only the exclusion from task scope goes.
- **The same exclusion in
  [`2026-11-10-1040-app-steps-executor-switch.md`](2026-11-10-1040-app-steps-executor-switch.md)**,
  which gave a second reason: the bases need the network to build and are
  part of the trust boundary. Both hold for building a base, which stays
  the operator's, and neither for editing its source: an edit reaches a
  ward only after the operator builds, probes, and pins it, and preflight
  refuses an agent image whose CLI disagrees with the daemon's pin.

## Rejected

- **A build path in the recipe or a ward.** A ward has no container
  runtime and no network, and building or adopting the image a task runs
  in is exactly what the agent bases being operator-built rules out.
  Agent-base upkeep (#2338) is the designed route to an automatic build,
  and it stays deferred.

## What the Owner Accepts

- **The pull request's review is the only check on an image edit** before
  the operator builds it. The recipe checks an agent-base change only
  through the two daemon tests above; nothing builds or lints an image.
- **A Claude CLI pin bump stays partly operator work.** The writer can edit
  the pin and the test's constants, but the new bundle's built-in plugins
  are measured from the binary, which a writer's room under the default
  `provider_only` egress can't fetch, and the adapter needs a live
  re-proof. Both stay operator steps until #2338.

## Revisit When

- Agent-base upkeep (#2338) is scheduled, which turns a merged image edit
  into an admitted digest without the operator.
- A writer's room gains a container runtime, or daemon code starts reading
  files under `images/`.
