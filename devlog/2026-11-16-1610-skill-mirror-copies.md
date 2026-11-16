# Repository Skills Mirror as Checked Copies, Not Symlinks

Owner decision of 2026-11-16, made when #2382's live run found that
Freeside couldn't seed its own repository.

## Decision

- **`.agents/skills/` holds plain copies of `.claude/skills/`,** in place
  of the relative symlinks #1513 added. Since #1513 (2026-09-23),
  `freeside-ai/freeside` tracks three symlinks
  (`.agents/skills/{exit-run,plan-wave,review-wave}`). The deputy's
  workspace seed refuses any tracked symlink, so ward conformance failed
  on every base since then: "tree path `.agents/skills/exit-run` has
  unsupported mode/type 120000/blob". Onboarding still completed, because
  the project-image build and proof don't seed through that path. The
  failure appeared only at the first deputy start after onboarding.
- **Rejected: implement #339 first** (seed a base that tracks a symlink).
  It's Tier A work in the materializer and the ward's host and guest tree
  digests, with its own refute-first review, and the live run would wait
  on it. #339 stays the real fix for repositories Freeside doesn't
  control.

## Consequence

Every skill edit now touches two folders. The duplication is the cost of
keeping Freeside able to work on its own repository while #339 is open.

Revisit when #339 lands: the copies can go back to symlinks.
