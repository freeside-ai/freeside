# Freeside's Recipe Runs the API Checks Without Compat

Owner decision of 2026-11-16, made when #2382's live run found that
Freeside couldn't finish its own onboarding (#3261).

## Decision

- **The recipe's api command names `lint digest policy`,** over running
  `compat` in the recipe now. `compat` (084732019) needs `oasdiff` and the
  merge base's spec. The project image ships `vacuum` but no `oasdiff`,
  and the offline proof gives a command no `.git`, so a bare
  `check.sh api` failed the image's proof and blocked every Freeside run.
- **Rejected: ship `oasdiff` in the image first.** That is a Tier A change
  to `daemon/internal/projectimage/` with its own review and note, and the
  live run would wait on it. It's #3262.
- **Naming the steps, not dropping the command,** keeps lint, digest, and
  policy in verification, and stops a step added to `check.sh api` later
  from joining the recipe without anyone proving it offline. That silent
  join is how `compat` broke the proof.

## Consequence

A breaking spec change that leaves `x-compatibility-level` unchanged now
passes Freeside's verification and fails only CI's `required`, so the
recipe no longer runs every check `required` aggregates (#2873's exit
item) until #3262 lands.

Revisit when #3262 merges: the recipe then runs `compat` as a history
command.
