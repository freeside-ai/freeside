# exit-root Renders the Deputy at the Fetched Tip

Every deputy pass of `scripts/exit-root.sh` now fetches
`repository.base_ref` into the repository checkout before it touches the
root, and renders that tip as `repository.base_sha`. A moved `main` asks for
no onboarding review and starts no deputy. exit-root withholds its `ready:`
and `submit=` lines (exit 1) while the passing `prelaunch -no-live` says no
recorded image serves the rendered tip. The owner chose this shape on
2026-11-26 for #3525, after #2382's routing rerun cost three manual steps
when `main` moved: a `git fetch` in the checkout, a re-pin with its deputy
start and review, and a hand-written `onboarded` record.

This revises the statement in
`devlog/2026-11-21-0756-onboard-approval-at-moved-tip.md` that exit-root
"pins the deputy to the commit a completed approval records." The state
file still records that commit as the base the approval bound; the deputy
no longer runs there. #3524's rule stays: only a moved template pin, a moved
review configuration digest, `--replace-intent`, or an incomplete approval
asks for a review.

## Chose Following the Tip Over Staying at the Reviewed Tip

#3524 kept the deputy at the last reviewed tip. The deputy's preflight
reads the base from the checkout, so a tip the operator hadn't fetched
failed `repository_base` and `implementer_image`, and the deputy ran below
the base the installed daemon would run at. Following the tip matches how
runs work since #3223 (they start at the observed tip of the base ref) and
needs no approval change: the trust profile covers the workflows and the
recipe, and a moved tip that changes neither leaves it valid. A tip that
does change them shows up in the deputy's own checks, as it would on the
installed daemon.

## Chose Following the Tip Over Re-Pinning and Re-Reviewing Each Move

#3525's first contract re-pinned and re-reviewed on every move of `main`.
That made each merge cost a render, a deputy start, and a review the owner
must answer, which is the cost the rerun showed. `repository.base_sha`
leaves `start_key` instead, because no adoption input reads the base, and
prelaunch reads it from the settings on every run.

## Chose a Required Base Over Removing the Key

A template may still set `repository.base_sha`, now as the base the pass
requires: a fetched tip that differs is refused (exit 2) on a new root and
a rerun. The fixture keeps its `fixture-baseline` value this way, so a pass
on an unreset fixture is refused instead of run and counted. A real run's
template leaves the key out.

## Chose Stopping Over Waiting for the Tip's Image

Readiness passes a base no recorded image serves, by design, since attempts
hold on it. exit-root stops instead of reporting ready, so the operator
doesn't submit into `image_upkeep_pending`. Image upkeep runs in a deputy
that composes a driver, at start and on tip moves, and exit-root's setup
start stops as soon as the deputy serves, so exit-root can't promise the
build finishes; waiting for it is a possible follow-up, not this unit. The
stop matches the evidence wording of preflight's `implementer_image`
selection, so it fails open if that wording changes.

## Chose Reviewing an Older Root Again Over Skipping It

A root onboarded before exit-root recorded what its review bound has no
`onboarded` record, so it had nothing to compare and skipped the check, and
kept an old trust profile with no review at the new base. With a completed
approval in place, it now reviews once more and records what that review
bound. A root with neither is a fresh one, which prelaunch's
`trust_profile` step onboards.

Revisit when the deputy's own preflight fetches or reads the tip, or when
exit-root is retired for attended runs.
