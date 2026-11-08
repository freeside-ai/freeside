# Freeside Manages Its Whole Repository

Issue #2874; plan revision 113; tracker #2873. The owner decided on
2026-11-08, in chat, that Freeside onboards `freeside-ai/freeside` for every
path but `images/`, `app/` included, instead of `daemon/` first and the rest
directory by directory.

Chose whole-repository onboarding over the per-directory sequence because
the directory seams cut through ordinary units, so partial onboarding made
the owner judge each task's edit set before handing it to Freeside. That
judgment is the interruption Freeside exists to remove.

## What It Overturns

- **Revision 92 item 2:** `daemon/` first, then `api/`, `scripts/`, and
  `docs/` as the baked recipe came to verify each networkless, with `app/`
  out until a macOS execution class existed.
- **Revision 99 item 6:** Freeside's `daemon/` onboarding as wave 9's exit
  proof.
- **Revision 96's order of work:** the Swift toolchain, the `FreesideCore`
  split, then "the executor as its own unit". Its executor design (the four
  confinements, pooled slots, in-process placement) stands.

## The Assumption That Changed

Revision 92 assumed a task's edit set stays inside one directory, so
onboarding one directory at a time would hand Freeside whole tasks. It
doesn't hold for Freeside's own units:

- A Tier A daemon change edits `AGENTS.md` (its Tier A path list).
- A daemon change with a client-visible effect edits `app/`.
- Any `api/openapi.yaml` change needs the generated client in `app/`.

With only `daemon/` onboarded, each of these reaches outside the onboarded
paths, so the owner has to see that coming and split the task or keep it
from Freeside. Onboarding more directories narrows the gap but never closes
it while `app/` stays out.

## Decisions

- **Chose every path but `images/` over a longer list of exclusions.**
  `images/` holds the agent bases, which stay operator-built (plan §5.7).
  Control-plane paths stay operator-authored inside the onboarded tree, as
  revision 92 already said; which paths the onboarding protects is #2883's
  input, recorded on that issue.
- **Chose the ad hoc tracker #2873 over adding the units to wave 9.** The
  owner first asked for them in wave 9. The work is ten units, a chain of
  seven serial ones before the live run, nine of ten Tier A, while wave 9's
  only open code unit is fenced. Listing them there would have held the
  1B.1 exit evaluation for the whole chain. Wave 9's exit proof now runs on
  a managed repository's issue (gh-imgup until Freeside is onboarded), and
  Freeside's own run moves to #2873.
- **Rejected a new wave 10 reusing the folded row.** That makes a numbered
  wave of one roadmap feature.
- **Rejected keeping partial onboarding as the steady state.** It keeps the
  per-task scope judgment.
- **Chose to name each recipe command's room in the plan.** One recipe must
  verify a repository whose checks span the project image and the macOS
  executor, and one image must carry Go and Swift. Reading the code at
  planning found three gaps revision 96 didn't name: the trusted recipe has
  no per-command execution target (#2875), a project image holds exactly
  one toolchain and #2183 refuses a Go module beside a Swift package
  (#2881), and the executor's slots and packet-filter anchor need
  administrator rights the daemon never holds (#2878). The plan states the
  commitments; those units carry the mechanism.
- **The execution-hosts tracker (#2161) is not a prerequisite.** Plan §5.7
  already places the executor in-process on the daemon host by default; a
  second Mac later shrinks the residual without changing the class.
- **Freeside's module split is done.** PR #2252 created `FreesideModel` and
  added `check.sh app linux`, the Linux step the recipe runs, so #2182
  closed as completed.

Revisit when the executor's conformance pass cannot be proven on the
reference host (#2879), since whole-repository onboarding then has no way to
verify `app/`'s Apple-only checks; or when agent-base upkeep (#2338) is
scheduled, since its maintenance task proposes edits in `images/`.
